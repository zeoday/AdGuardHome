package home

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AdguardTeam/AdGuardHome/internal/aghhttp"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/miekg/dns"
)

// validateFilterURL validates the filter list URL or file name.
func validateFilterURL(urlStr string) (err error) {
	if filepath.IsAbs(urlStr) {
		_, err = os.Stat(urlStr)
		if err != nil {
			return fmt.Errorf("checking filter file: %w", err)
		}

		return nil
	}

	url, err := url.ParseRequestURI(urlStr)
	if err != nil {
		return fmt.Errorf("checking filter url: %w", err)
	}

	if s := url.Scheme; s != schemeHTTP && s != schemeHTTPS {
		return fmt.Errorf("checking filter url: invalid scheme %q", s)
	}

	return nil
}

type filterAddJSON struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Whitelist bool   `json:"whitelist"`
}

func (f *Filtering) handleFilteringAddURL(w http.ResponseWriter, r *http.Request) {
	fj := filterAddJSON{}
	err := json.NewDecoder(r.Body).Decode(&fj)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "Failed to parse request body json: %s", err)

		return
	}

	err = validateFilterURL(fj.URL)
	if err != nil {
		err = fmt.Errorf("invalid url: %s", err)
		aghhttp.Error(r, w, http.StatusBadRequest, "%s", err)

		return
	}

	// Check for duplicates
	if filterExists(fj.URL) {
		aghhttp.Error(r, w, http.StatusBadRequest, "Filter URL already added -- %s", fj.URL)

		return
	}

	// Set necessary properties
	filt := filter{
		Enabled: true,
		URL:     fj.URL,
		Name:    fj.Name,
		white:   fj.Whitelist,
	}
	filt.ID = assignUniqueFilterID()

	// Download the filter contents
	ok, err := f.update(&filt)
	if err != nil {
		aghhttp.Error(
			r,
			w,
			http.StatusBadRequest,
			"Couldn't fetch filter from url %s: %s",
			filt.URL,
			err,
		)

		return
	}

	if !ok {
		aghhttp.Error(
			r,
			w,
			http.StatusBadRequest,
			"Filter at the url %s is invalid (maybe it points to blank page?)",
			filt.URL,
		)

		return
	}

	// URL is assumed valid so append it to filters, update config, write new
	// file and reload it to engines.
	if !filterAdd(filt) {
		aghhttp.Error(r, w, http.StatusBadRequest, "Filter URL already added -- %s", filt.URL)

		return
	}

	onConfigModified()
	enableFilters(true)

	_, err = fmt.Fprintf(w, "OK %d rules\n", filt.RulesCount)
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "Couldn't write body: %s", err)
	}
}

func (f *Filtering) handleFilteringRemoveURL(w http.ResponseWriter, r *http.Request) {
	type request struct {
		URL       string `json:"url"`
		Whitelist bool   `json:"whitelist"`
	}

	req := request{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "failed to parse request body json: %s", err)

		return
	}

	config.Lock()
	filters := &config.Filters
	if req.Whitelist {
		filters = &config.WhitelistFilters
	}

	var deleted filter
	var newFilters []filter
	for _, f := range *filters {
		if f.URL != req.URL {
			newFilters = append(newFilters, f)

			continue
		}

		deleted = f
		path := f.Path()
		err = os.Rename(path, path+".old")
		if err != nil {
			log.Error("deleting filter %q: %s", path, err)
		}
	}

	*filters = newFilters
	config.Unlock()

	onConfigModified()
	enableFilters(true)

	// NOTE: The old files "filter.txt.old" aren't deleted.  It's not really
	// necessary, but will require the additional complicated code to run
	// after enableFilters is done.
	//
	// TODO(a.garipov): Make sure the above comment is true.

	_, err = fmt.Fprintf(w, "OK %d rules\n", deleted.RulesCount)
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "couldn't write body: %s", err)
	}
}

type filterURLReqData struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Enabled bool   `json:"enabled"`
}

type filterURLReq struct {
	Data      *filterURLReqData `json:"data"`
	URL       string            `json:"url"`
	Whitelist bool              `json:"whitelist"`
}

func (f *Filtering) handleFilteringSetURL(w http.ResponseWriter, r *http.Request) {
	fj := filterURLReq{}
	err := json.NewDecoder(r.Body).Decode(&fj)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "json decode: %s", err)

		return
	}

	if fj.Data == nil {
		err = errors.Error("data cannot be null")
		aghhttp.Error(r, w, http.StatusBadRequest, "%s", err)

		return
	}

	err = validateFilterURL(fj.Data.URL)
	if err != nil {
		err = fmt.Errorf("invalid url: %s", err)
		aghhttp.Error(r, w, http.StatusBadRequest, "%s", err)

		return
	}

	filt := filter{
		Enabled: fj.Data.Enabled,
		Name:    fj.Data.Name,
		URL:     fj.Data.URL,
	}
	status := f.filterSetProperties(fj.URL, filt, fj.Whitelist)
	if (status & statusFound) == 0 {
		http.Error(w, "URL doesn't exist", http.StatusBadRequest)
		return
	}
	if (status & statusURLExists) != 0 {
		http.Error(w, "URL already exists", http.StatusBadRequest)
		return
	}

	onConfigModified()

	restart := (status & statusEnabledChanged) != 0
	if (status&statusUpdateRequired) != 0 && fj.Data.Enabled {
		// download new filter and apply its rules
		flags := filterRefreshBlocklists
		if fj.Whitelist {
			flags = filterRefreshAllowlists
		}
		nUpdated, _ := f.refreshFilters(flags, true)
		// if at least 1 filter has been updated, refreshFilters() restarts the filtering automatically
		// if not - we restart the filtering ourselves
		restart = false
		if nUpdated == 0 {
			restart = true
		}
	}

	if restart {
		enableFilters(true)
	}
}

func (f *Filtering) handleFilteringSetRules(w http.ResponseWriter, r *http.Request) {
	// This use of ReadAll is safe, because request's body is now limited.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "Failed to read request body: %s", err)

		return
	}

	config.UserRules = strings.Split(string(body), "\n")
	onConfigModified()
	enableFilters(true)
}

func (f *Filtering) handleFilteringRefresh(w http.ResponseWriter, r *http.Request) {
	type Req struct {
		White bool `json:"whitelist"`
	}
	type Resp struct {
		Updated int `json:"updated"`
	}
	resp := Resp{}
	var err error

	req := Req{}
	err = json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "json decode: %s", err)

		return
	}

	flags := filterRefreshBlocklists
	if req.White {
		flags = filterRefreshAllowlists
	}
	func() {
		// Temporarily unlock the Context.controlLock because the
		// f.refreshFilters waits for it to be unlocked but it's
		// actually locked in ensure wrapper.
		//
		// TODO(e.burkov):  Reconsider this messy syncing process.
		Context.controlLock.Unlock()
		defer Context.controlLock.Lock()

		resp.Updated, err = f.refreshFilters(flags|filterRefreshForce, false)
	}()
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "%s", err)

		return
	}

	js, err := json.Marshal(resp)
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "json encode: %s", err)

		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(js)
}

type filterJSON struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	LastUpdated string `json:"last_updated,omitempty"`
	ID          int64  `json:"id"`
	RulesCount  uint32 `json:"rules_count"`
	Enabled     bool   `json:"enabled"`
}

type filteringConfig struct {
	Filters          []filterJSON `json:"filters"`
	WhitelistFilters []filterJSON `json:"whitelist_filters"`
	UserRules        []string     `json:"user_rules"`
	Interval         uint32       `json:"interval"` // in hours
	Enabled          bool         `json:"enabled"`
}

func filterToJSON(f filter) filterJSON {
	fj := filterJSON{
		ID:         f.ID,
		Enabled:    f.Enabled,
		URL:        f.URL,
		Name:       f.Name,
		RulesCount: uint32(f.RulesCount),
	}

	if !f.LastUpdated.IsZero() {
		fj.LastUpdated = f.LastUpdated.Format(time.RFC3339)
	}

	return fj
}

// Get filtering configuration
func (f *Filtering) handleFilteringStatus(w http.ResponseWriter, r *http.Request) {
	resp := filteringConfig{}
	config.RLock()
	resp.Enabled = config.DNS.FilteringEnabled
	resp.Interval = config.DNS.FiltersUpdateIntervalHours
	for _, f := range config.Filters {
		fj := filterToJSON(f)
		resp.Filters = append(resp.Filters, fj)
	}
	for _, f := range config.WhitelistFilters {
		fj := filterToJSON(f)
		resp.WhitelistFilters = append(resp.WhitelistFilters, fj)
	}
	resp.UserRules = config.UserRules
	config.RUnlock()

	jsonVal, err := json.Marshal(resp)
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "json encode: %s", err)

		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(jsonVal)
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "http write: %s", err)
	}
}

// Set filtering configuration
func (f *Filtering) handleFilteringConfig(w http.ResponseWriter, r *http.Request) {
	req := filteringConfig{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		aghhttp.Error(r, w, http.StatusBadRequest, "json decode: %s", err)

		return
	}

	if !checkFiltersUpdateIntervalHours(req.Interval) {
		aghhttp.Error(r, w, http.StatusBadRequest, "Unsupported interval")

		return
	}

	func() {
		config.Lock()
		defer config.Unlock()

		config.DNS.FilteringEnabled = req.Enabled
		config.DNS.FiltersUpdateIntervalHours = req.Interval
	}()

	onConfigModified()
	enableFilters(true)
}

type checkHostRespRule struct {
	Text         string `json:"text"`
	FilterListID int64  `json:"filter_list_id"`
}

type checkHostResp struct {
	Reason string `json:"reason"`

	// Rule is the text of the matched rule.
	//
	// Deprecated: Use Rules[*].Text.
	Rule string `json:"rule"`

	Rules []*checkHostRespRule `json:"rules"`

	// for FilteredBlockedService:
	SvcName string `json:"service_name"`

	// for Rewrite:
	CanonName string   `json:"cname"`    // CNAME value
	IPList    []net.IP `json:"ip_addrs"` // list of IP addresses

	// FilterID is the ID of the rule's filter list.
	//
	// Deprecated: Use Rules[*].FilterListID.
	FilterID int64 `json:"filter_id"`
}

func (f *Filtering) handleCheckHost(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	host := q.Get("name")

	setts := Context.dnsFilter.GetConfig()
	setts.FilteringEnabled = true
	setts.ProtectionEnabled = true
	Context.dnsFilter.ApplyBlockedServices(&setts, nil, true)
	result, err := Context.dnsFilter.CheckHost(host, dns.TypeA, &setts)
	if err != nil {
		aghhttp.Error(
			r,
			w,
			http.StatusInternalServerError,
			"couldn't apply filtering: %s: %s",
			host,
			err,
		)

		return
	}

	resp := checkHostResp{}
	resp.Reason = result.Reason.String()
	resp.SvcName = result.ServiceName
	resp.CanonName = result.CanonName
	resp.IPList = result.IPList

	if len(result.Rules) > 0 {
		resp.FilterID = result.Rules[0].FilterListID
		resp.Rule = result.Rules[0].Text
	}

	resp.Rules = make([]*checkHostRespRule, len(result.Rules))
	for i, r := range result.Rules {
		resp.Rules[i] = &checkHostRespRule{
			FilterListID: r.FilterListID,
			Text:         r.Text,
		}
	}

	js, err := json.Marshal(resp)
	if err != nil {
		aghhttp.Error(r, w, http.StatusInternalServerError, "json encode: %s", err)

		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(js)
}

// RegisterFilteringHandlers - register handlers
func (f *Filtering) RegisterFilteringHandlers() {
	httpRegister(http.MethodGet, "/control/filtering/status", f.handleFilteringStatus)
	httpRegister(http.MethodPost, "/control/filtering/config", f.handleFilteringConfig)
	httpRegister(http.MethodPost, "/control/filtering/add_url", f.handleFilteringAddURL)
	httpRegister(http.MethodPost, "/control/filtering/remove_url", f.handleFilteringRemoveURL)
	httpRegister(http.MethodPost, "/control/filtering/set_url", f.handleFilteringSetURL)
	httpRegister(http.MethodPost, "/control/filtering/refresh", f.handleFilteringRefresh)
	httpRegister(http.MethodPost, "/control/filtering/set_rules", f.handleFilteringSetRules)
	httpRegister(http.MethodGet, "/control/filtering/check_host", f.handleCheckHost)
}

func checkFiltersUpdateIntervalHours(i uint32) bool {
	return i == 0 || i == 1 || i == 12 || i == 1*24 || i == 3*24 || i == 7*24
}
