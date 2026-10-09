// acv-prices sweeps Air Canada Vacations package search across destination
// groups, departure dates and trip durations, writing every result to a CSV.
package main

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	tokenURL  = "https://vacations.aircanada.com/session-authentication/token/jwt"
	searchURL = "https://vacations-api.aircanada.com/packages-search/search"
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"
	pageSize  = 20
)

// destGroups maps a group name to the destId and dest list the website sends.
// The API requires a dest list; these were copied from the site's own requests
// and can be overridden with -mexico-dests / -caribbean-dests if they change.
var destGroups = map[string]struct{ ID, Dests string }{
	"caribbean": {"all-caribbean", "8,10,14,18,25,27,28,29,30,33,34,35,36,38,39,40,46,60,62,73,76,78,90,188,251,1843,4244,710451,712372,1341400,1341882,1447566,SDQ"},
	"mexico":    {"all-mexico", "2,7,9,17,24,44,69,77,86,156,254,277,278,291,1375,2478,3049111,TQO"},
}

type config struct {
	orig, childAges, amenities, cabin, out string
	adults                                 int
	groups                                 []string
	from, to                               time.Time
	durations                              []int
	delay, jitter, breakFor                time.Duration
	maxPages, breakEvery, maxFailures      int
}

func main() {
	var (
		cfg                        config
		groups, from, to, durStr   string
		mexicoDests, caribbeanDest string
	)
	flag.StringVar(&cfg.orig, "orig", "YYZ", "origin airport")
	flag.StringVar(&groups, "groups", "caribbean,mexico", "comma-separated destination groups (caribbean, mexico)")
	flag.StringVar(&from, "from", "", "first departure date, YYYY-MM-DD (required)")
	flag.StringVar(&to, "to", "", "last departure date, YYYY-MM-DD (required)")
	flag.StringVar(&durStr, "durations", "7", "comma-separated trip lengths in nights, e.g. 5,7,10")
	flag.IntVar(&cfg.adults, "adults", 2, "number of adults")
	flag.StringVar(&cfg.childAges, "child-ages", "", "comma-separated child ages, e.g. 3,5,7,9")
	flag.StringVar(&cfg.amenities, "amenities", "ALL_INCLUSIVE", "famenities filter (empty for none)")
	flag.StringVar(&cfg.cabin, "cabin", "Y", "cabin class")
	flag.IntVar(&cfg.maxPages, "max-pages", 0, "max result pages (20 each) per search; 0 = all")
	flag.DurationVar(&cfg.delay, "delay", 8*time.Second, "base wait between requests")
	flag.DurationVar(&cfg.jitter, "jitter", 7*time.Second, "random extra wait added to -delay")
	flag.IntVar(&cfg.breakEvery, "break-every", 50, "take a longer break after this many requests; 0 = never")
	flag.DurationVar(&cfg.breakFor, "break", 3*time.Minute, "length of the periodic break")
	flag.IntVar(&cfg.maxFailures, "max-failures", 3, "stop the run after this many failed searches in a row")
	flag.StringVar(&cfg.out, "out", "acv-prices.csv", "output CSV path")
	flag.StringVar(&mexicoDests, "mexico-dests", "", "override the dest id list for mexico")
	flag.StringVar(&caribbeanDest, "caribbean-dests", "", "override the dest id list for caribbean")
	flag.Parse()

	var err error
	if cfg.from, err = time.Parse("2006-01-02", from); err != nil {
		log.Fatalf("-from: %v", err)
	}
	if cfg.to, err = time.Parse("2006-01-02", to); err != nil {
		log.Fatalf("-to: %v", err)
	}
	if cfg.to.Before(cfg.from) {
		log.Fatal("-to is before -from")
	}
	for _, s := range splitList(durStr) {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			log.Fatalf("bad duration %q", s)
		}
		cfg.durations = append(cfg.durations, n)
	}
	for _, g := range splitList(groups) {
		if _, ok := destGroups[g]; !ok {
			log.Fatalf("unknown group %q", g)
		}
		cfg.groups = append(cfg.groups, g)
	}
	setDests("mexico", mexicoDests)
	setDests("caribbean", caribbeanDest)
	for _, g := range cfg.groups {
		if destGroups[g].Dests == "" {
			log.Fatalf("no dest list for %q: copy the dest= value from a %s search on the site and pass it with -%s-dests", g, g, g)
		}
	}

	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func setDests(group, dests string) {
	if dests != "" {
		g := destGroups[group]
		g.Dests = dests
		destGroups[group] = g
	}
}

func run(cfg config) error {
	f, err := os.Create(cfg.out)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write(csvHeader); err != nil {
		return err
	}

	jar, _ := cookiejar.New(nil)
	c := &client{http: &http.Client{Timeout: 60 * time.Second, Jar: jar}, cfg: cfg}

	days := int(cfg.to.Sub(cfg.from).Hours()/24) + 1
	total := days * len(cfg.durations) * len(cfg.groups)
	n, failures := 0, 0
	for d := cfg.from; !d.After(cfg.to); d = d.AddDate(0, 0, 1) {
		for _, nights := range cfg.durations {
			for _, g := range cfg.groups {
				n++
				ret := d.AddDate(0, 0, nights)
				log.Printf("[%d/%d] %s %s +%dn", n, total, g, d.Format("2006-01-02"), nights)
				rows, err := c.searchAll(g, d, ret)
				if err != nil {
					// One failed combination shouldn't sink a long sweep, but a
					// run of them likely means we're being blocked: stop hitting it.
					log.Printf("  error: %v", err)
					if failures++; cfg.maxFailures > 0 && failures >= cfg.maxFailures {
						return fmt.Errorf("stopping after %d failed searches in a row; results so far are in %s", failures, cfg.out)
					}
					continue
				}
				failures = 0
				log.Printf("  %d packages", len(rows))
				for _, r := range rows {
					if err := w.Write(r); err != nil {
						return err
					}
				}
				w.Flush()
				if err := w.Error(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type client struct {
	http     *http.Client
	cfg      config
	token    string
	tokenExp time.Time
	lastReq  time.Time
	requests int
}

// wait spaces out requests by delay + random jitter, with a longer break every
// breakEvery requests.
func (c *client) wait() {
	if !c.lastReq.IsZero() {
		gap := c.cfg.delay
		if c.cfg.jitter > 0 {
			gap += time.Duration(rand.Int64N(int64(c.cfg.jitter)))
		}
		if c.cfg.breakEvery > 0 && c.requests%c.cfg.breakEvery == 0 {
			gap = c.cfg.breakFor
			log.Printf("  %d requests made; taking a %s break", c.requests, gap)
		}
		if d := time.Until(c.lastReq.Add(gap)); d > 0 {
			time.Sleep(d)
		}
	}
	c.lastReq = time.Now()
	c.requests++
}

func browserHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", "https://vacations.aircanada.com")
	req.Header.Set("Referer", "https://vacations.aircanada.com/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-ACV-Form-Factor", "desktop")
}

// ensureToken fetches a guest JWT, refreshing 30s before it expires
// (they're issued with a 5-minute lifetime).
func (c *client) ensureToken() error {
	if c.token != "" && time.Now().Add(30*time.Second).Before(c.tokenExp) {
		return nil
	}
	tok, err := c.fetchToken()
	if err != nil {
		return fmt.Errorf("fetching token: %w", err)
	}
	c.token = tok
	c.tokenExp = jwtExpiry(tok)
	log.Printf("  got token (expires %s)", c.tokenExp.Format(time.TimeOnly))
	return nil
}

func (c *client) fetchToken() (string, error) {
	c.wait()
	req, err := http.NewRequest(http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", err
	}
	browserHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	}
	return extractToken(body)
}

func extractToken(body []byte) (string, error) {
	var r struct {
		Error       any    `json:"error"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if r.Error != nil || r.AccessToken == "" {
		return "", fmt.Errorf("no token in response: %.200s", body)
	}
	return r.AccessToken, nil
}

func jwtExpiry(tok string) time.Time {
	parts := strings.Split(tok, ".")
	if len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 {
				return time.Unix(claims.Exp, 0)
			}
		}
	}
	return time.Now().Add(4 * time.Minute)
}

var errUnauthorized = errors.New("unauthorized")

// searchAll pages through every result for one combination.
func (c *client) searchAll(group string, dep, ret time.Time) ([][]string, error) {
	var rows [][]string
	for page, offset := 0, 0; ; page++ {
		res, err := c.searchWithRetry(group, dep, ret, offset)
		if err != nil {
			return rows, err
		}
		rows = append(rows, toRows(group, dep, ret, res)...)
		offset += len(res.Results.Packages)
		if len(res.Results.Packages) == 0 || offset >= res.Results.MetaData.TotalAvailable ||
			(c.cfg.maxPages > 0 && page+1 >= c.cfg.maxPages) {
			return rows, nil
		}
	}
}

func (c *client) searchWithRetry(group string, dep, ret time.Time, offset int) (*searchResponse, error) {
	backoff := time.Minute
	for attempt := 1; ; attempt++ {
		if err := c.ensureToken(); err != nil {
			return nil, err
		}
		res, retryAfter, retryable, err := c.search(group, dep, ret, offset)
		if err == nil {
			return res, nil
		}
		if errors.Is(err, errUnauthorized) {
			c.token = ""
		}
		if !retryable || attempt == 4 {
			return nil, err
		}
		wait := max(backoff, retryAfter)
		log.Printf("  %v; retrying in %s", err, wait)
		time.Sleep(wait)
		backoff *= 2
	}
}

func (c *client) search(group string, dep, ret time.Time, offset int) (res *searchResponse, retryAfter time.Duration, retryable bool, err error) {
	g := destGroups[group]
	q := url.Values{}
	q.Set("orig", c.cfg.orig)
	q.Set("destId", g.ID)
	q.Set("destType", "DESTINATIONGROUP")
	if g.Dests != "" {
		q.Set("dest", g.Dests)
	}
	q.Set("adults", strconv.Itoa(c.cfg.adults))
	if c.cfg.childAges != "" {
		q.Set("child-ages", c.cfg.childAges)
	}
	q.Set("dep-date", dep.Format("20060102"))
	q.Set("ret-date", ret.Format("20060102"))
	q.Set("cabin", c.cfg.cabin)
	q.Set("lang", "en")
	q.Set("rooms", "1")
	q.Set("hotels", "")
	q.Set("sort", "priceasc")
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(pageSize))
	q.Set("applySponsorPlacement", "false")
	if c.cfg.amenities != "" {
		q.Set("famenities", c.cfg.amenities)
	}

	c.wait()
	req, err := http.NewRequest(http.MethodGet, searchURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, 0, false, err
	}
	browserHeaders(req)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, true, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, 0, true, fmt.Errorf("HTTP %d: %w", resp.StatusCode, errUnauthorized)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return nil, time.Duration(secs) * time.Second, true, fmt.Errorf("HTTP %d: %.200s", resp.StatusCode, body)
	case resp.StatusCode != http.StatusOK:
		return nil, 0, false, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, body)
	}
	res = &searchResponse{}
	if err := json.Unmarshal(body, res); err != nil {
		return nil, 0, false, fmt.Errorf("decoding response: %w", err)
	}
	return res, 0, false, nil
}

type searchResponse struct {
	Results struct {
		Packages []struct {
			ID          string `json:"id"`
			Information struct {
				Destination       string  `json:"destination"`
				MealPlanName      string  `json:"mealPlanName"`
				DistanceToAirport float64 `json:"distanceToAirportKm"`
				Refundable        bool    `json:"refundable"`
				TransferIncluded  bool    `json:"transferIncluded"`
			} `json:"information"`
			HotelID          string `json:"hotelId"`
			RoomID           string `json:"roomId"`
			RoomsLeft        int    `json:"roomsLeft"`
			SelectedOptionID struct {
				ItineraryID string `json:"itineraryId"`
			} `json:"selectedOptionIds"`
			Pricing struct {
				PerPaxType []struct {
					PaxType  string  `json:"paxType"`
					PaxAge   int     `json:"paxAge"`
					WasPrice float64 `json:"wasPrice"`
					Total    float64 `json:"total"`
				} `json:"perPaxType"`
				GrandTotal float64 `json:"grandTotal"`
			} `json:"pricing"`
			Promotions []struct {
				Description string `json:"description"`
			} `json:"promotions"`
		} `json:"packages"`
		MetaData struct {
			TotalAvailable int `json:"totalAvailable"`
		} `json:"metaData"`
	} `json:"results"`
	Hotels map[string]struct {
		Name         string  `json:"name"`
		StarCategory float64 `json:"starCategory"`
		Destination  string  `json:"destination"`
		Chain        struct {
			Name string `json:"name"`
		} `json:"hotelChain"`
		Rating struct {
			Value   float64 `json:"value"`
			Reviews int     `json:"reviews"`
		} `json:"rating"`
		Rooms map[string]struct {
			Name string `json:"name"`
		} `json:"rooms"`
	} `json:"hotels"`
	Itineraries map[string]struct {
		Segments []struct {
			Bound         string `json:"bound"`
			DepartureTime string `json:"departureTime"`
			ArrivalTime   string `json:"arrivalTime"`
			FlightNumber  string `json:"flightNumber"`
			CarrierCode   string `json:"carrierCode"`
			Connections   int    `json:"numConnections"`
		} `json:"segments"`
	} `json:"itineraries"`
}

var csvHeader = []string{
	"group", "dep_date", "ret_date", "nights", "dest_airport", "destination",
	"hotel_id", "hotel", "chain", "stars", "ta_rating", "ta_reviews", "room",
	"meal_plan", "grand_total", "adult_pp_total", "adult_pp_was", "child_totals",
	"refundable", "transfer_included", "km_to_airport", "rooms_left",
	"outbound", "return", "promotions", "package_id",
}

func toRows(group string, dep, ret time.Time, res *searchResponse) [][]string {
	nights := int(ret.Sub(dep).Hours() / 24)
	var rows [][]string
	for _, p := range res.Results.Packages {
		h := res.Hotels[p.HotelID]
		var adultTotal, adultWas string
		var kids, promos []string
		for _, pp := range p.Pricing.PerPaxType {
			if pp.PaxType == "A" {
				adultTotal = money(pp.Total)
				if pp.WasPrice > 0 {
					adultWas = money(pp.WasPrice)
				}
			} else {
				kids = append(kids, fmt.Sprintf("%d:%s", pp.PaxAge, money(pp.Total)))
			}
		}
		for _, pr := range p.Promotions {
			promos = append(promos, pr.Description)
		}
		var outbound, inbound string
		for _, s := range res.Itineraries[p.SelectedOptionID.ItineraryID].Segments {
			desc := fmt.Sprintf("%s%s %s-%s", s.CarrierCode, s.FlightNumber, s.DepartureTime, s.ArrivalTime)
			if s.Connections > 0 {
				desc += fmt.Sprintf(" (%d stop)", s.Connections)
			}
			if s.Bound == "O" {
				outbound = desc
			} else {
				inbound = desc
			}
		}
		roomsLeft := ""
		if p.RoomsLeft > 0 {
			roomsLeft = strconv.Itoa(p.RoomsLeft)
		}
		rows = append(rows, []string{
			group, dep.Format("2006-01-02"), ret.Format("2006-01-02"), strconv.Itoa(nights),
			p.Information.Destination, h.Destination,
			p.HotelID, h.Name, h.Chain.Name, fmt.Sprint(h.StarCategory),
			fmt.Sprint(h.Rating.Value), strconv.Itoa(h.Rating.Reviews), strings.TrimSpace(h.Rooms[p.RoomID].Name),
			p.Information.MealPlanName, money(p.Pricing.GrandTotal), adultTotal, adultWas, strings.Join(kids, " "),
			strconv.FormatBool(p.Information.Refundable), strconv.FormatBool(p.Information.TransferIncluded),
			strconv.FormatFloat(p.Information.DistanceToAirport, 'f', 1, 64), roomsLeft,
			outbound, inbound, strings.Join(promos, "; "), p.ID,
		})
	}
	return rows
}

func money(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func splitList(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
