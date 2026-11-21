// Copyright 2026 ziul510
// SPDX-License-Identifier: Apache-2.0

package geo

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ziul510/GSXCommsGo/internal/assets"
)

//go:embed cities.tsv.gz
var index []byte

//go:embed countries.json
var countryMap []byte

type city struct {
	x, y, z         float64
	country, region string
}

type Position struct{ Country, Region string }

// Label uses the nearest indexed region and country for display
func (p *Position) Label() string {
	if p == nil || p.Country == "" {
		return "unavailable"
	}
	country := mapping.Names[p.Country]
	if country == "" {
		country = p.Country
	}
	if p.Region != "" && !strings.EqualFold(p.Region, country) {
		return p.Region + ", " + country
	}
	return country
}

// Decompress the index on the first position lookup and reuse it for later polls
var cities = sync.OnceValues(load)
var mapping struct {
	Names   map[string]string            `json:"COUNTRY_NAMES"`
	Country map[string]string            `json:"COUNTRY_TO_ACCENT"`
	Regions map[string]map[string]string `json:"REGION_ACCENT_OVERRIDES"`
}

func xyz(lat, lon float64) (float64, float64, float64) {
	lat *= math.Pi / 180
	lon *= math.Pi / 180
	sin, cos := math.Sincos(lat)
	sinLon, cosLon := math.Sincos(lon)
	// Use the squared eccentricity of the WGS84 ellipsoid
	const eccentricity = 0.00669437999014
	radius := 6378137 / math.Sqrt(1-eccentricity*sin*sin)
	return radius * cos * cosLon, radius * cos * sinLon, radius * (1 - eccentricity) * sin
}

func load() ([]city, error) {
	// Use the embedded gzip index so city lookups can run offline
	archive, err := gzip.NewReader(bytes.NewReader(index))
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	rows := bufio.NewScanner(archive)
	result := make([]city, 0, 140000)
	for rows.Scan() {
		// The generator writes latitude, longitude, country code, then region name
		part := strings.Split(rows.Text(), "\t")
		if len(part) != 4 {
			return nil, errors.New("invalid city record")
		}
		lat, a := strconv.ParseFloat(part[0], 64)
		lon, b := strconv.ParseFloat(part[1], 64)
		if a != nil || b != nil {
			return nil, errors.New("invalid city coordinates")
		}
		x, y, z := xyz(lat, lon)
		// Store Cartesian coordinates so later aircraft lookups avoid recomputing every city
		result = append(result, city{x, y, z, part[2], part[3]})
	}
	return result, rows.Err()
}

func Nearest(lat, lon float64) (Position, error) {
	// Reject invalid coordinates before scanning the city index
	if math.IsNaN(lat) || math.IsNaN(lon) || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return Position{}, errors.New("invalid aircraft coordinates")
	}
	data, err := cities()
	if err != nil {
		return Position{}, err
	}
	// Cartesian distances work across the date line without wrapping longitude
	x, y, z := xyz(lat, lon)
	best := math.Inf(1)
	var result Position
	for _, row := range data {
		dx, dy, dz := row.x-x, row.y-y, row.z-z
		// Squared distance gives the same ordering without taking a square root per city
		if distance := dx*dx + dy*dy + dz*dz; distance < best {
			best = distance
			result = Position{row.country, row.region}
		}
	}
	return result, nil
}

var stockNames map[string]string

func init() {
	if err := json.Unmarshal(countryMap, &mapping); err != nil {
		panic(err)
	}
	raw, err := assets.Files.ReadFile(assets.AccentMappingsFile)
	if err != nil {
		panic(err)
	}
	stockNames = parseOverrides(raw)
	// Fill missing country codes from the bundled names
	for code, name := range mapping.Names {
		if mapping.Country[code] == "" {
			mapping.Country[code] = stockNames[strings.ToLower(name)]
		}
	}
}

// AccentCandidates preserves the legacy country name preference before regional fallbacks
func AccentCandidates(pos Position, overrides map[string]string) []string {
	name := strings.ToLower(mapping.Names[pos.Country])
	return []string{
		overrides[strings.ToLower(pos.Country)], overrides[name], stockNames[name],
		mapping.Regions[pos.Country][strings.ToLower(pos.Region)], mapping.Country[pos.Country],
	}
}

// Load the optional user override file
func Overrides(dir string) map[string]string {
	raw, err := os.ReadFile(filepath.Join(dir, assets.AccentMappingsFile))
	if err != nil {
		return nil
	}
	return parseOverrides(raw)
}

// Normalize names and ignore incomplete entries before doing override lookups
func parseOverrides(raw []byte) map[string]string {
	entries, err := assets.DecodeAccentMappings(raw)
	if err != nil {
		return nil
	}
	values := map[string]string{}
	for _, entry := range entries {
		name := strings.ToLower(strings.TrimSpace(entry.Country))
		if name != "" && entry.Accent != "" {
			values[name] = entry.Accent
		}
	}
	return values
}
