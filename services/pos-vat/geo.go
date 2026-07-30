package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// GeoAttribution is the capture-time state/LGA attribution result.
type GeoAttribution struct {
	State  string `json:"state"`
	LGA    string `json:"lga"`
	Ward   string `json:"ward,omitempty"`
	Source string `json:"source"` // geo-svc|embedded
}

type GeoClient interface {
	AttributePoint(lat, lon float64) (GeoAttribution, error)
}

// HTTPGeoClient calls the core geo svc (POST /v1/attribution/point).
type HTTPGeoClient struct{ Base string }

func (c *HTTPGeoClient) AttributePoint(lat, lon float64) (GeoAttribution, error) {
	body, _ := json.Marshal(map[string]float64{"lat": lat, "lon": lon})
	cli := &http.Client{Timeout: 1200 * time.Millisecond}
	resp, err := cli.Post(c.Base+"/v1/attribution/point", "application/json", bytes.NewReader(body))
	if err != nil {
		// graceful degradation to embedded coarse lookup
		g, gerr := EmbeddedGeo{}.AttributePoint(lat, lon)
		if gerr == nil {
			g.Source = "embedded-fallback"
		}
		return g, gerr
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return EmbeddedGeo{}.AttributePoint(lat, lon)
	}
	var out GeoAttribution
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return EmbeddedGeo{}.AttributePoint(lat, lon)
	}
	out.Source = "geo-svc"
	return out, nil
}

// EmbeddedGeo is the offline fallback: coarse state bounding boxes plus
// nearest-centroid LGA guess for the major commercial states. Honesty tag:
// coarse — production attribution comes from geo-rs point-in-polygon.
type EmbeddedGeo struct{}

type bbox struct {
	state              string
	minLat, maxLat     float64
	minLon, maxLon     float64
}

// Coarse bounding boxes (lat 4.0–13.9N, lon 2.7–14.7E for Nigeria).
var stateBoxes = []bbox{
	{"Lagos", 6.35, 6.75, 2.65, 4.35},
	{"Ogun", 6.55, 7.95, 2.65, 4.65},
	{"Oyo", 7.05, 9.25, 2.65, 4.85},
	{"Osun", 6.95, 8.15, 4.05, 5.15},
	{"Ondo", 5.85, 7.95, 4.35, 6.05},
	{"Edo", 5.75, 7.55, 5.05, 6.75},
	{"Delta", 4.95, 6.55, 5.05, 6.85},
	{"Rivers", 4.35, 5.75, 6.35, 7.65},
	{"Bayelsa", 4.35, 5.65, 5.65, 6.65},
	{"Akwa Ibom", 4.55, 5.55, 7.45, 8.45},
	{"Cross River", 4.75, 6.95, 7.85, 9.45},
	{"Anambra", 5.75, 6.85, 6.55, 7.55},
	{"Enugu", 5.95, 7.15, 7.05, 7.95},
	{"Imo", 5.15, 6.05, 6.75, 7.55},
	{"Abia", 4.85, 5.95, 7.15, 8.05},
	{"Ebonyi", 5.65, 6.85, 7.55, 8.55},
	{"FCT", 8.25, 9.25, 6.45, 7.65},
	{"Nasarawa", 7.65, 8.95, 7.15, 8.95},
	{"Plateau", 8.15, 10.35, 8.35, 10.65},
	{"Benue", 6.55, 8.25, 7.45, 9.95},
	{"Kogi", 6.55, 8.75, 5.55, 7.95},
	{"Kwara", 7.95, 9.65, 2.75, 6.05},
	{"Niger", 8.15, 11.35, 3.55, 7.35},
	{"Kaduna", 9.15, 11.55, 6.15, 8.75},
	{"Kano", 10.35, 12.45, 7.75, 10.45},
	{"Katsina", 11.15, 13.35, 6.85, 9.25},
	{"Jigawa", 11.15, 13.15, 8.05, 10.45},
	{"Sokoto", 12.15, 13.85, 3.95, 6.75},
	{"Zamfara", 10.85, 13.25, 5.55, 7.25},
	{"Kebbi", 10.15, 12.85, 3.55, 6.25},
	{"Bauchi", 9.65, 12.55, 8.55, 11.05},
	{"Gombe", 9.85, 11.35, 10.55, 12.15},
	{"Yobe", 10.55, 13.35, 9.75, 12.55},
	{"Borno", 10.05, 13.75, 11.55, 14.75},
	{"Adamawa", 7.45, 10.95, 11.35, 13.75},
	{"Taraba", 6.45, 9.55, 9.45, 11.75},
	{"Ekiti", 7.15, 8.15, 4.75, 5.75},
}

type lgaCentroid struct {
	lga       string
	lat, lon  float64
}

// Major commercial LGAs (centroid approximations, coarse).
var lgaCentroids = map[string][]lgaCentroid{
	"Lagos": {
		{"Ikeja", 6.60, 3.35}, {"Eti-Osa", 6.45, 3.60}, {"Lagos Island", 6.45, 3.40},
		{"Surulere", 6.50, 3.35}, {"Alimosho", 6.61, 3.25}, {"Ikorodu", 6.62, 3.51},
		{"Lekki", 6.44, 3.70},
	},
	"FCT": {
		{"Abuja Municipal", 9.06, 7.49}, {"Gwagwalada", 8.94, 7.08}, {"Kuje", 8.88, 7.23},
	},
	"Kano": {
		{"Kano Municipal", 12.00, 8.52}, {"Nassarawa", 11.99, 8.55}, {"Fagge", 12.02, 8.52},
	},
	"Rivers": {
		{"Port Harcourt", 4.82, 7.01}, {"Obio-Akpor", 4.90, 6.98}, {"Eleme", 4.79, 7.12},
	},
	"Oyo": {
		{"Ibadan North", 7.40, 3.90}, {"Ibadan South-West", 7.37, 3.88}, {"Egbeda", 7.38, 3.95},
	},
	"Kaduna": {
		{"Kaduna North", 10.55, 7.44}, {"Kaduna South", 10.48, 7.42}, {"Zaria", 11.07, 7.70},
	},
	"Enugu": {
		{"Enugu North", 6.46, 7.51}, {"Enugu South", 6.43, 7.50},
	},
	"Anambra": {
		{"Onitsha North", 6.15, 6.79}, {"Awka South", 6.21, 7.07}, {"Nnewi North", 6.02, 6.92},
	},
	"Delta": {
		{"Warri South", 5.52, 5.75}, {"Oshimili South", 6.18, 6.70},
	},
	"Borno": {
		{"Maiduguri", 11.85, 13.15},
	},
}

func (EmbeddedGeo) AttributePoint(lat, lon float64) (GeoAttribution, error) {
	if lat < 3.5 || lat > 14.2 || lon < 2.5 || lon > 15.0 {
		return GeoAttribution{}, fmt.Errorf("point outside Nigeria coarse bounds")
	}
	state := ""
	best := math.MaxFloat64
	// prefer the box whose centre is nearest (overlapping boxes)
	for _, b := range stateBoxes {
		if lat >= b.minLat && lat <= b.maxLat && lon >= b.minLon && lon <= b.maxLon {
			d := math.Hypot(lat-(b.minLat+b.maxLat)/2, lon-(b.minLon+b.maxLon)/2)
			if d < best {
				best, state = d, b.state
			}
		}
	}
	if state == "" {
		return GeoAttribution{}, fmt.Errorf("no state match for point")
	}
	lga := state + " (unresolved)"
	if cents, ok := lgaCentroids[state]; ok {
		bd := math.MaxFloat64
		for _, c := range cents {
			d := math.Hypot(lat-c.lat, lon-c.lon)
			if d < bd {
				bd, lga = d, c.lga
			}
		}
	}
	return GeoAttribution{State: state, LGA: lga, Source: "embedded"}, nil
}
