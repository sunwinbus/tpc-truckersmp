package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/avoinescu/ets2-decrypt/bsii"
	"github.com/avoinescu/ets2-decrypt/decoder"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/windows/registry"
)

const (
	ETS2Name = "Euro Truck Simulator 2"
	ATSName  = "American Truck Simulator"

	debounceDelay  = 900 * time.Millisecond
	stableInterval = 250 * time.Millisecond
	stableAttempts = 16
)

var (
	fieldLineRE = regexp.MustCompile(
		`^(\s*)([A-Za-z0-9_]+)(?:\[(\d+)\])?\s*:\s*(.*?)\s*$`,
	)

	percentEnvRE = regexp.MustCompile(`%([^%]+)%`)

	wearFields = map[string]bool{
		"trailer_body_wear":           true,
		"trailer_body_wear_unfixable": true,
		"chassis_wear":                true,
		"chassis_wear_unfixable":      true,
		"engine_wear":                 true,
		"engine_wear_unfixable":       true,
		"transmission_wear":           true,
		"transmission_wear_unfixable": true,
		"cabin_wear":                  true,
		"cabin_wear_unfixable":        true,
		"wheels_wear":                 true,
		"wheels_wear_unfixable":       true,
	}
)

type app struct {
	watcher      *fsnotify.Watcher
	documentsDir string

	// The following fields are accessed only by the watcher goroutine.
	watchedDirs  map[string]bool
	profileRoots map[string]bool
	pending      map[string]time.Time

	jobs chan string
}

func main() {
	documentsDir, err := getDocumentsPath()
	fatalIf(err)

	watcher, err := fsnotify.NewWatcher()
	fatalIf(err)
	defer watcher.Close()

	a := &app{
		watcher:      watcher,
		documentsDir: documentsDir,
		watchedDirs:  make(map[string]bool),
		profileRoots: make(map[string]bool),
		pending:      make(map[string]time.Time),
		jobs:         make(chan string, 64),
	}

	fmt.Println("================= TPC For TruckersMP =================")
	fmt.Println("Usage: 0.Type g_debug_camera 1 in console (only once)")
	fmt.Println("       1.Alt+F12 to save coordinate of freecam")
	fmt.Println("       2.Make a quicksave & reload 5 seconds later")
	fmt.Println("Email: sunwinbus@ets666.com | Discord: sunwinbus#1343")
	fmt.Println("Thanks to DF-41 for his great idea!")
	fmt.Println("======================================================")

	fatalIf(a.initializeWatch())

	go a.worker()
	a.watchLoop()
}

func fatalIf(err error) {
	if err == nil {
		return
	}

	fmt.Fprintln(os.Stderr, "Fatal error:", err)
	fmt.Fprintln(os.Stderr, "Press Enter to exit...")

	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(1)
}

func getDocumentsPath() (string, error) {
	key, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`,
		registry.QUERY_VALUE,
	)
	if err != nil {
		return "", fmt.Errorf(
			"failed to read Windows Documents directory registry key: %w",
			err,
		)
	}
	defer key.Close()

	path, _, err := key.GetStringValue("Personal")
	if err != nil {
		return "", fmt.Errorf(
			"failed to read Personal registry value: %w",
			err,
		)
	}

	path = strings.TrimSpace(path)

	path = percentEnvRE.ReplaceAllStringFunc(path, func(token string) string {
		name := strings.Trim(token, "%")
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
		return token
	})

	path = os.ExpandEnv(path)

	if path == "" {
		return "", errors.New("Windows Documents directory is empty")
	}

	return filepath.Clean(path), nil
}

// initializeWatch initializes the ETS2 and ATS profile watchers.
func (a *app) initializeWatch() error {
	for _, gameName := range []string{ETS2Name, ATSName} {
		profilesRoot := filepath.Join(
			a.documentsDir,
			gameName,
			"profiles",
		)

		if isDir(profilesRoot) {
			if err := a.addProfileRoot(profilesRoot); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *app) addProfileRoot(root string) error {
	root = filepath.Clean(root)
	a.profileRoots[root] = true

	if err := a.addWatchDir(root); err != nil {
		return err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf(
			"failed to read save directory %s: %w",
			root,
			err,
		)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			if err := a.addProfileDir(
				filepath.Join(root, entry.Name()),
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func (a *app) addProfileDir(profileDir string) error {
	saveDir := filepath.Join(profileDir, "save")

	if isDir(saveDir) {
		return a.addSaveDir(saveDir)
	}

	// Watch the profile directory to detect save directory creation.
	return a.addWatchDir(profileDir)
}

func (a *app) addSaveDir(saveDir string) error {
	if err := a.addWatchDir(saveDir); err != nil {
		return err
	}

	quickSaveDir := filepath.Join(saveDir, "quicksave")

	if isDir(quickSaveDir) {
		return a.addQuickSaveDir(quickSaveDir)
	}

	return nil
}

func (a *app) addQuickSaveDir(dir string) error {
	return a.addWatchDir(dir)
}

func (a *app) addWatchDir(dir string) error {
	dir = filepath.Clean(dir)

	if a.watchedDirs[dir] {
		return nil
	}

	if !isDir(dir) {
		return nil
	}

	if err := a.watcher.Add(dir); err != nil {
		return fmt.Errorf(
			"failed to add directory watcher %s: %w",
			dir,
			err,
		)
	}

	a.watchedDirs[dir] = true

	fmt.Println("Monitoring:", dir)

	return nil
}

// watchLoop handles filesystem events and dispatches save processing jobs.
func (a *app) watchLoop() {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case ev, ok := <-a.watcher.Events:
			if !ok {
				return
			}

			a.handleEvent(ev)

		case err, ok := <-a.watcher.Errors:
			if !ok {
				return
			}

			fmt.Fprintln(os.Stderr, "File watcher error:", err)

		case now := <-ticker.C:
			for path, due := range a.pending {
				if !now.Before(due) {
					select {
					case a.jobs <- path:
						delete(a.pending, path)

					default:
						// Retry later if the queue is full.
						a.pending[path] = now.Add(debounceDelay)
					}
				}
			}
		}
	}
}

// handleEvent processes filesystem events.
func (a *app) handleEvent(ev fsnotify.Event) {
	name := filepath.Clean(ev.Name)

	// Detect newly created save and quicksave directories.
	if ev.Op&(fsnotify.Create|fsnotify.Rename) != 0 && isDir(name) {
		base := strings.ToLower(filepath.Base(name))

		switch base {
		case "save":
			if err := a.addSaveDir(name); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}

		case "quicksave":
			if err := a.addQuickSaveDir(name); err != nil {
				fmt.Fprintln(os.Stderr, err)
			}

		default:
			if a.profileRoots[filepath.Clean(filepath.Dir(name))] {
				if err := a.addProfileDir(name); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		}
	}

	// Only monitor quicksave/game.sii.
	if !strings.EqualFold(filepath.Base(name), "game.sii") ||
		!strings.EqualFold(
			filepath.Base(filepath.Dir(name)),
			"quicksave",
		) {
		return
	}

	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Chmod) == 0 {
		return
	}

	a.pending[name] = time.Now().Add(debounceDelay)
}

// worker processes save files sequentially.
func (a *app) worker() {
	lastProcessed := make(map[string][32]byte)

	for path := range a.jobs {
		changed, hash, err := a.processFile(path, lastProcessed)

		if err != nil {
			fmt.Fprintf(
				os.Stderr,
				"Failed to process %s: %v\n",
				path,
				err,
			)
			continue
		}

		if changed {
			fmt.Println("Updated:", path)
		}

		if hash != ([32]byte{}) {
			lastProcessed[path] = hash
		}
	}
}

// processFile waits for a stable save, decodes it, edits its SII data,
// and writes the result only when actual modifications are necessary.
func (a *app) processFile(
	path string,
	lastProcessed map[string][32]byte,
) (bool, [32]byte, error) {
	var zeroHash [32]byte

	// Wait until the game finishes writing the save.
	rawData, err := waitForStableFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, zeroHash, nil
		}

		return false, zeroHash, err
	}

	// Hash the original on-disk file.
	beforeDecryptHash := sha256.Sum256(rawData)

	// Skip files that have already been processed.
	if previous, ok := lastProcessed[path]; ok &&
		previous == beforeDecryptHash {
		return false, beforeDecryptHash, nil
	}

	// Decode SII in memory.
	data, err := decodeSii(rawData)
	if err != nil {
		return false, zeroHash, err
	}

	// Determine which game's camera coordinate file to use.
	gameName := gameNameFromPath(path)
	camsPath := filepath.Join(
		a.documentsDir,
		gameName,
		"cams.txt",
	)

	camsData, err := os.ReadFile(camsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, beforeDecryptHash, nil
		}

		return false, zeroHash, fmt.Errorf(
			"failed to read camera coordinate file %s: %w",
			camsPath,
			err,
		)
	}

	// Parse camera coordinates.
	location, rotation, err := parseCamsCoordinate(camsData)
	if err != nil {
		return false, beforeDecryptHash, err
	}

	// Apply SII modifications.
	output, changed, err := editSII(data, location, rotation)
	if err != nil {
		return false, zeroHash, err
	}

	if !changed {
		return false, beforeDecryptHash, nil
	}

	// Atomically replace the original save file.
	if err := writeFileAtomic(path, output); err != nil {
		return false, zeroHash, err
	}

	// The file on disk now contains edited plaintext.
	return true, sha256.Sum256(output), nil
}

// decodeSii decodes SiiN, BSII, and ScsC save formats.
func decodeSii(raw []byte) ([]byte, error) {
	data, kind, err := decoder.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to decode SII save: %w", err)
	}

	// BSII requires an additional binary decoding step.
	if kind == decoder.KindBinary {
		data, err = bsii.Decode(data)
		if err != nil {
			return nil, fmt.Errorf(
				"failed to decode BSII binary data: %w",
				err,
			)
		}
	}

	// Verify that the final output is valid SiiNunit plaintext.
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("SiiNunit")) {
		return nil, errors.New(
			"decoded save is not valid SiiNunit plaintext",
		)
	}

	return data, nil
}

// gameNameFromPath determines whether a save belongs to ETS2 or ATS.
func gameNameFromPath(path string) string {
	clean := filepath.Clean(path)
	lowerPath := strings.ToLower(clean)

	for _, name := range []string{ETS2Name, ATSName} {
		if strings.Contains(lowerPath, strings.ToLower(name)) {
			return name
		}
	}

	// Watched paths originate only from ETS2/ATS profile directories.
	return ETS2Name
}

// waitForStableFile waits until the target file has stopped changing.
func waitForStableFile(path string) ([]byte, error) {
	var previousSize int64 = -1
	var previousModTime time.Time
	stableCount := 0
	var lastErr error

	for i := 0; i < stableAttempts; i++ {
		info, err := os.Stat(path)
		if err != nil {
			lastErr = err
			stableCount = 0
			time.Sleep(stableInterval)
			continue
		}

		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf(
				"target is not a regular file: %s",
				path,
			)
		}

		if info.Size() == previousSize &&
			info.ModTime().Equal(previousModTime) {
			stableCount++
		} else {
			stableCount = 0
		}

		previousSize = info.Size()
		previousModTime = info.ModTime()
		lastErr = nil

		if stableCount >= 2 {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				lastErr = readErr
				stableCount = 0
				time.Sleep(stableInterval)
				continue
			}

			// Check metadata again after reading.
			after, statErr := os.Stat(path)

			if statErr == nil &&
				after.Size() == info.Size() &&
				after.ModTime().Equal(info.ModTime()) {
				return data, nil
			}

			stableCount = 0
		}

		time.Sleep(stableInterval)
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, fmt.Errorf(
		"file did not become stable within %s; postponing processing",
		time.Duration(stableAttempts)*stableInterval,
	)
}

// parseCamsCoordinate extracts the latest camera coordinate record.
func parseCamsCoordinate(
	data []byte,
) (location, rotation string, err error) {
	lines := strings.Split(
		strings.ReplaceAll(string(data), "\r\n", "\n"),
		"\n",
	)

	var lastLine string

	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = strings.TrimSpace(lines[i])
			break
		}
	}

	if lastLine == "" {
		return "", "", errors.New(
			"cams.txt is empty; no usable coordinates found",
		)
	}

	// Position X/Y/Z, quaternion X/Y/Z/W, and FOV.
	rawFields := strings.Split(lastLine, ";")
	fields := make([]string, 0, len(rawFields))

	for _, field := range rawFields {
		field = strings.TrimSpace(field)

		if field != "" {
			fields = append(fields, field)
		}
	}

	if len(fields) != 8 {
		return "", "", fmt.Errorf(
			"invalid cams.txt record: expected 8 numeric fields, got %d: %q",
			len(fields),
			lastLine,
		)
	}

	// Validate all fields, including FOV.
	for i, field := range fields {
		if _, parseErr := parseSIIFloat(field); parseErr != nil {
			return "", "", fmt.Errorf(
				"invalid cams.txt field %d: %w",
				i+1,
				parseErr,
			)
		}
	}

	location = strings.Join(fields[:3], ", ")
	rotation = fields[3] + "; " + strings.Join(fields[4:7], ", ")

	if _, parseErr := parsePlacement(
		"(" + location + ") (" + rotation + ")",
	); parseErr != nil {
		return "", "", fmt.Errorf(
			"invalid coordinate format in cams.txt: %w",
			parseErr,
		)
	}

	fmt.Printf("Target: (%s) (%s)\n", location, rotation)

	return location, rotation, nil
}

// editSII modifies SII data while preserving original line endings.
func editSII(
	data []byte,
	location, rotation string,
) ([]byte, bool, error) {
	cameraPlacement, err := parsePlacement(
		"(0, 0, 0) (" + rotation + ")",
	)
	if err != nil {
		return nil, false, fmt.Errorf(
			"invalid camera rotation: %w",
			err,
		)
	}

	text := string(data)

	lineEnding := "\n"

	if strings.Contains(text, "\r\n") {
		lineEnding = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}

	// Preserve the trailing empty line.
	lines := strings.Split(text, "\n")
	removeLine := make([]bool, len(lines))
	changed := false

	depth := 0
	objectStart := -1
	trailerArrayActive := false
	trailerAssigned := false
	trailerChanged := false

	for i, original := range lines {
		line := strings.TrimSuffix(original, "\r")

		// SII data objects are direct children of the SiiNunit root.
		if strings.Contains(line, "{") {
			if depth == 1 {
				objectStart = i
				trailerArrayActive = false
				trailerAssigned = false
				trailerChanged = false
			}

			depth += strings.Count(line, "{")
		}

		match := fieldLineRE.FindStringSubmatch(line)

		if len(match) == 5 {
			indent, key, indexText, value :=
				match[1], match[2], match[3], match[4]

			// Check whether this player_vehicles object references a trailer.
			if key == "trailer" && indexText == "" {
				trailerValue := strings.TrimSpace(value)

				trailerAssigned = trailerValue != "" &&
					!strings.EqualFold(trailerValue, "null") &&
					!strings.EqualFold(trailerValue, `""`)
			}

			switch {
			case key == "stored_vehicle_placement" &&
				indexText == "":
				isDefault, parseErr := isDefaultPlacement(value)

				if parseErr != nil {
					return nil, false, fmt.Errorf(
						"invalid stored_vehicle_placement format on line %d: %w",
						i+1,
						parseErr,
					)
				}

				if !isDefault {
					targetPlacement := "(" + location + ") (" + rotation + ")"

					if strings.TrimSpace(value) != targetPlacement {
						lines[i] = indent + key + ": " + targetPlacement
						changed = true
					}
				}

			case key == "stored_trailer_placements" &&
				indexText == "":
				count, parseErr := strconv.Atoi(strings.TrimSpace(value))

				if parseErr != nil || count < 0 {
					return nil, false, fmt.Errorf(
						"invalid stored_trailer_placements count on line %d: %q",
						i+1,
						value,
					)
				}

				trailerArrayActive = count > 0

			case key == "stored_trailer_placements" &&
				indexText != "" &&
				trailerArrayActive:
				parsed, parseErr := parsePlacement(value)

				if parseErr != nil {
					return nil, false, fmt.Errorf(
						"invalid stored_trailer_placements[%s] format on line %d: %w",
						indexText,
						i+1,
						parseErr,
					)
				}

				rotationMatches := len(parsed.rotation) ==
					len(cameraPlacement.rotation)

				if rotationMatches {
					for j := range parsed.rotation {
						if math.Abs(
							parsed.rotation[j]-cameraPlacement.rotation[j],
						) > 1e-7 {
							rotationMatches = false
							break
						}
					}
				}

				if !isZeroVector(parsed.position) ||
					!rotationMatches {
					lines[i] = indent +
						key + "[" + indexText +
						"]: (0, 0, 0) (" + rotation + ")"

					trailerChanged = true
					changed = true
				}
			}

			// Set every fuel_relative value below 1 to 1.
			// Supports decimal and IEEE754 hexadecimal representations.
			if key == "fuel_relative" {
				fuelValue, parseErr := parseSIIFloat(value)

				if parseErr != nil {
					return nil, false, fmt.Errorf(
						"invalid fuel_relative value on line %d: %w",
						i+1,
						parseErr,
					)
				}

				if fuelValue < 1 {
					fieldName := key

					if indexText != "" {
						fieldName += "[" + indexText + "]"
					}

					lines[i] = indent + fieldName + ": 1"
					changed = true
				}
			}

			// Reset every nav_node_position in the entire SII file.
			if key == "nav_node_position" {
				if strings.TrimSpace(value) != "(0, 0, 0)" {
					fieldName := key

					if indexText != "" {
						fieldName += "[" + indexText + "]"
					}

					lines[i] = indent + fieldName + ": (0, 0, 0)"
					changed = true
				}
			}

			// Zero all wear fields and remove indexed wheel-wear entries.
			if wearFields[key] {
				if indexText != "" &&
					(key == "wheels_wear" ||
						key == "wheels_wear_unfixable") {
					removeLine[i] = true
					changed = true

				} else if indexText == "" &&
					strings.TrimSpace(value) != "0" {
					lines[i] = indent + key + ": 0"
					changed = true
				}
			}
		}

		// Set stored_trailer_attached in the same modified trailer object.
		if strings.Contains(line, "}") {
			closeCount := strings.Count(line, "}")

			if depth == 2 &&
				objectStart >= 0 &&
				trailerAssigned &&
				trailerChanged {
				for j := objectStart + 1; j < i; j++ {
					m := fieldLineRE.FindStringSubmatch(lines[j])

					if len(m) != 5 ||
						m[3] != "" ||
						m[2] != "stored_trailer_attached" {
						continue
					}

					if !strings.EqualFold(
						strings.TrimSpace(m[4]),
						"true",
					) {
						lines[j] = m[1] +
							"stored_trailer_attached: true"
						changed = true
					}

					break
				}
			}

			depth -= closeCount

			if depth < 0 {
				depth = 0
			}

			if depth < 2 {
				objectStart = -1
				trailerArrayActive = false
				trailerAssigned = false
				trailerChanged = false
			}
		}
	}

	if !changed {
		return data, false, nil
	}

	filteredLines := make([]string, 0, len(lines))

	for i, line := range lines {
		if !removeLine[i] {
			filteredLines = append(filteredLines, line)
		}
	}

	output := strings.Join(filteredLines, "\n")

	if lineEnding == "\r\n" {
		output = strings.ReplaceAll(output, "\n", "\r\n")
	}

	return []byte(output), true, nil
}

type parsedPlacement struct {
	position     []float64
	rotation     []float64
	rotationText string
}

// parsePlacement parses SII position and quaternion rotation values.
func parsePlacement(value string) (parsedPlacement, error) {
	var result parsedPlacement

	value = strings.TrimSpace(value)

	separator := strings.Index(value, ") (")
	if separator < 0 {
		return result, fmt.Errorf(
			"missing separator between position and rotation: %q",
			value,
		)
	}

	positionText := strings.TrimSpace(value[:separator+1])
	rotationText := strings.TrimSpace(value[separator+2:])

	if !strings.HasPrefix(positionText, "(") ||
		!strings.HasSuffix(positionText, ")") ||
		!strings.HasPrefix(rotationText, "(") ||
		!strings.HasSuffix(rotationText, ")") {
		return result, fmt.Errorf(
			"incomplete coordinate parentheses: %q",
			value,
		)
	}

	positionText = strings.TrimSpace(
		positionText[1 : len(positionText)-1],
	)

	rotationTextInner := strings.TrimSpace(
		rotationText[1 : len(rotationText)-1],
	)

	positionParts := strings.Split(positionText, ",")
	rotationParts := strings.SplitN(rotationTextInner, ";", 2)

	if len(rotationParts) != 2 {
		return result, fmt.Errorf(
			"rotation requires a semicolon after the first component: %q",
			value,
		)
	}

	rotationParts = append(
		[]string{rotationParts[0]},
		strings.Split(rotationParts[1], ",")...,
	)

	if len(positionParts) != 3 || len(rotationParts) != 4 {
		return result, fmt.Errorf(
			"position requires 3 components and rotation requires 4 components: %q",
			value,
		)
	}

	for _, part := range positionParts {
		v, err := parseSIIFloat(part)
		if err != nil {
			return result, err
		}

		result.position = append(result.position, v)
	}

	for _, part := range rotationParts {
		v, err := parseSIIFloat(part)
		if err != nil {
			return result, err
		}

		result.rotation = append(result.rotation, v)
	}

	result.rotationText = rotationText

	return result, nil
}

// parseSIIFloat supports decimal and IEEE754 hexadecimal SII values.
func parseSIIFloat(token string) (float64, error) {
	token = strings.TrimSpace(token)

	if strings.HasPrefix(token, "&") {
		raw := strings.TrimPrefix(token, "&")

		bits, err := strconv.ParseUint(raw, 16, 32)
		if err != nil {
			return 0, fmt.Errorf(
				"invalid IEEE754 hexadecimal value %q: %w",
				token,
				err,
			)
		}

		v := float64(math.Float32frombits(uint32(bits)))

		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf(
				"floating-point value is not finite: %q",
				token,
			)
		}

		return v, nil
	}

	v, err := strconv.ParseFloat(token, 64)

	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf(
			"invalid floating-point value %q",
			token,
		)
	}

	return v, nil
}

func isZeroVector(values []float64) bool {
	if len(values) != 3 {
		return false
	}

	for _, v := range values {
		if math.Abs(v) > 1e-7 {
			return false
		}
	}

	return true
}

// isDefaultPlacement checks whether the position is zero
// and the quaternion is the identity quaternion.
func isDefaultPlacement(value string) (bool, error) {
	parsed, err := parsePlacement(value)
	if err != nil {
		return false, err
	}

	if !isZeroVector(parsed.position) ||
		len(parsed.rotation) != 4 {
		return false, nil
	}

	return math.Abs(parsed.rotation[0]-1) <= 1e-7 &&
		math.Abs(parsed.rotation[1]) <= 1e-7 &&
		math.Abs(parsed.rotation[2]) <= 1e-7 &&
		math.Abs(parsed.rotation[3]) <= 1e-7, nil
}

// writeFileAtomic writes data to a temporary file and atomically
// replaces the original save file.
func writeFileAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".game.sii-*.tmp")
	if err != nil {
		return fmt.Errorf(
			"failed to create temporary save file: %w",
			err,
		)
	}

	tmpName := tmp.Name()

	cleanup := func() {
		_ = os.Remove(tmpName)
	}

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()

		return fmt.Errorf(
			"failed to write temporary save file: %w",
			err,
		)
	}

	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()

		return fmt.Errorf(
			"failed to flush temporary save file: %w",
			err,
		)
	}

	if err = tmp.Close(); err != nil {
		cleanup()
		return err
	}

	if err = os.Chmod(tmpName, info.Mode()); err != nil {
		cleanup()
		return err
	}

	// The temporary file is in the same directory as the original.
	if err = os.Rename(tmpName, path); err != nil {
		cleanup()

		return fmt.Errorf(
			"failed to replace save file: %w",
			err,
		)
	}

	return nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
