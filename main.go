// Copyright (C) 2026-present ScyllaDB
// SPDX-License-Identifier: LicenseRef-ScyllaDB-Source-Available-1.0

// Porcupine-based linearizability checker for ScyllaDB consistency tests
// (SC tables, LWT, schema changes, etc.).

// Reads a JSON-lines history from stdin, checks linearizability of a per-key
// integer register model (supporting indeterminate/timeout outcomes), and
// outputs a JSON result to stdout.  On failure, writes an interactive HTML
// visualization to a temporary file whose path is included in the JSON output.
//
// Usage:
//
//	porcupine_checker < history.jsonl
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anishathalye/porcupine"
)

// EventKind distinguishes call vs return events.
type EventKind int

const (
	KindUnknown EventKind = iota
	KindCall
	KindReturn
)

func (k *EventKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "call":
		*k = KindCall
	case "return":
		*k = KindReturn
	default:
		*k = KindUnknown
	}
	return nil
}

// OpType distinguishes read vs write operations.
type OpType int

const (
	OpUnknown OpType = iota
	OpRead
	OpWrite
)

func (o *OpType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch s {
	case "read":
		*o = OpRead
	case "write":
		*o = OpWrite
	default:
		*o = OpUnknown
	}
	return nil
}

// StatusCode represents the outcome of an operation.
type StatusCode int

const (
	StatusNone StatusCode = iota
	StatusOk
	StatusFail
	StatusUnknown
)

func (s *StatusCode) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	switch str {
	case "ok":
		*s = StatusOk
	case "fail":
		*s = StatusFail
	case "unknown":
		*s = StatusUnknown
	default:
		*s = StatusNone
	}
	return nil
}

func (s StatusCode) String() string {
	switch s {
	case StatusOk:
		return "ok"
	case StatusFail:
		return "fail"
	case StatusUnknown:
		return "unknown"
	default:
		return "?"
	}
}

// rawEvent is a single event line from the JSON-lines history file.
type rawEvent struct {
	ID       int        `json:"id"`
	ClientID int        `json:"client_id"`
	Kind     EventKind  `json:"kind"`
	Op       OpType     `json:"op"`
	Key      int        `json:"key"`
	Value    int        `json:"value"`
	TimeNs   int64      `json:"time_ns"`
	Status   StatusCode `json:"status"`
}

// opInput is attached to each Porcupine Operation as Input.
type opInput struct {
	Op    OpType
	Key   int
	Value int // value to write; unused for reads
}

// opOutput is attached to each Porcupine Operation as Output.
type opOutput struct {
	Value  int        // value read; unused for writes
	Status StatusCode
}

// checkerResult is the JSON written to stdout.
type checkerResult struct {
	Valid         bool   `json:"valid"`
	KeysChecked   int    `json:"keys_checked"`
	TotalOps      int    `json:"total_ops"`
	Error         string `json:"error,omitempty"`
	Visualization string `json:"visualization,omitempty"`
}

// registerModel returns a Porcupine Model for a single-key integer register.
//
// No Partition function — partitioning by key is done manually in main()
// before this model is invoked, so each call to CheckEventsVerbose already
// receives events for exactly one key.
//
// State is an int (current register value, initially 0).
// NondeterministicModel handles timeout (StatusUnknown) writes that may or
// may not have been applied.
func registerModel() porcupine.Model {
	nm := porcupine.NondeterministicModel{
		// Partition intentionally omitted: we partition by key manually.
		Init: func() []interface{} {
			return []interface{}{0}
		},
		Step: func(state interface{}, input interface{}, output interface{}) []interface{} {
			st := state.(int)
			inp := input.(opInput)
			out := output.(opOutput)

			switch inp.Op {
			case OpWrite:
				switch out.Status {
				case StatusOk:
					return []interface{}{inp.Value}
				case StatusUnknown:
					// Timeout: write may or may not have been applied.
					return []interface{}{st, inp.Value}
				case StatusFail:
					return []interface{}{st}
				}
			case OpRead:
				switch out.Status {
				case StatusOk:
					if out.Value == st {
						return []interface{}{st}
					}
					return nil // illegal: read returned wrong value
				case StatusUnknown, StatusFail:
					// Read didn't complete or failed; state unchanged.
					return []interface{}{st}
				}
			}
			return nil
		},
		DescribeOperation: func(input interface{}, output interface{}) string {
			inp := input.(opInput)
			out := output.(opOutput)
			switch inp.Op {
			case OpWrite:
				return fmt.Sprintf("w(k=%d, v=%d) [%s]", inp.Key, inp.Value, out.Status)
			case OpRead:
				if out.Status == StatusOk {
					return fmt.Sprintf("r()->%d", out.Value)
				}
				return fmt.Sprintf("r() [%s]", out.Status)
			}
			return "?"
		},
		DescribeState: func(state interface{}) string {
			return fmt.Sprintf("%v", state)
		},
	}
	return nm.ToModel()
}

func main() {
	outputDir := flag.String("output-dir", "", "directory for debug artifacts")
	flag.Parse()

	historyData, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading history from stdin: %v\n", err)
		os.Exit(2)
	}

	if len(historyData) == 0 {
		fmt.Fprintf(os.Stderr, "error: no input data\n")
		os.Exit(2)
	}

	if *outputDir != "" {
		if err := os.MkdirAll(*outputDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "error creating output dir %s: %v\n", *outputDir, err)
			os.Exit(2)
		}
	}

	rawEvents, err := parseHistory(bytes.NewReader(historyData))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing history from stdin: %v\n", err)
		os.Exit(2)
	}

	events, keyByID, nUnpaired, err := buildEvents(rawEvents)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building events: %v\n", err)
		os.Exit(2)
	}
	if nUnpaired > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d unpaired call events treated as indeterminate\n", nUnpaired)
	}

	// Partition events by key manually.
	// This avoids relying on NondeterministicModel.Partition, which is not
	// wired through ToModel() and therefore has no effect on CheckEventsVerbose.
	byKey := make(map[int][]porcupine.Event)
	totalOps := 0
	for _, ev := range events {
		key := keyByID[ev.Id]
		byKey[key] = append(byKey[key], ev)
		if ev.Kind == porcupine.CallEvent {
			totalOps++
		}
	}

	sortedKeys := make([]int, 0, len(byKey))
	for k := range byKey {
		sortedKeys = append(sortedKeys, k)
	}
	slices.Sort(sortedKeys)

	if totalOps == 0 {
		fmt.Fprintf(os.Stderr, "warning: history is empty, nothing to check\n")
	}

	model := registerModel()

	res := checkerResult{
		Valid:       true,
		KeysChecked: len(sortedKeys),
		TotalOps:    totalOps,
	}

loop:
	for _, key := range sortedKeys {
		keyEvents := byKey[key]
		checkRes, vizInfo := porcupine.CheckEventsVerbose(model, keyEvents, 10*time.Second)

		switch checkRes {
		case porcupine.Illegal:
			res.Valid = false
			res.Error = fmt.Sprintf("linearizability violation detected on key %d", key)

			var vizPath string
			if *outputDir != "" {
				vizPath = filepath.Join(*outputDir, fmt.Sprintf("porcupine_viz_key_%d.html", key))
			} else {
				vizFile, tmpErr := os.CreateTemp("", "porcupine_viz_*.html")
				if tmpErr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not create visualization file: %v\n", tmpErr)
				} else {
					vizPath = vizFile.Name()
					vizFile.Close()
				}
			}

			if vizPath != "" {
				if err := porcupine.VisualizePath(model, vizInfo, vizPath); err != nil {
					fmt.Fprintf(os.Stderr, "warning: visualization write failed: %v\n", err)
				} else {
					res.Visualization = vizPath
					fmt.Fprintf(os.Stderr, "visualization written to %s\n", vizPath)
				}
			}
			// Stop at first violating key — one visualization is enough.
			break loop

		case porcupine.Unknown:
			// Per-key timeout: report but continue checking other keys so we
			// don't silently skip them.  Mark overall result as unknown only
			// if no harder violation is found.
			if res.Valid {
				res.Valid = false
				res.Error = fmt.Sprintf("linearizability check timed out on key %d (result unknown)", key)
			}
		}
	}

	out, err := json.Marshal(res)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling result: %v\n", err)
		os.Exit(2)
	}

	fmt.Println(string(out))

	if *outputDir != "" && !res.Valid {
		historyPath := filepath.Join(*outputDir, "history.jsonl")
		if err := os.WriteFile(historyPath, historyData, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write history artifact %s: %v\n", historyPath, err)
		}

		resultPath := filepath.Join(*outputDir, "result.json")
		prettyOut, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not marshal pretty result: %v\n", err)
		} else if err := os.WriteFile(resultPath, prettyOut, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write result artifact %s: %v\n", resultPath, err)
		}
	}

	if !res.Valid {
		os.Exit(1)
	}
}

func parseHistory(r io.Reader) ([]rawEvent, error) {
	var events []rawEvent
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 1<<20)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		var ev rawEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		events = append(events, ev)
	}
	return events, scanner.Err()
}

// buildEvents converts raw JSON-lines events into Porcupine events and returns
// a keyByID map so callers can associate any event (including returns, which
// carry no key in their Value) back to its original key.
//
// Unpaired calls (in-flight at test end) get synthetic StatusUnknown return events.
func buildEvents(rawEvents []rawEvent) ([]porcupine.Event, map[int]int, int, error) {
	slices.SortFunc(rawEvents, func(a, b rawEvent) int {
		return cmp.Compare(a.TimeNs, b.TimeNs)
	})

	var events []porcupine.Event
	keyByID := make(map[int]int)  // op_id → key
	unpaired := make(map[int]*rawEvent)

	for i := range rawEvents {
		ev := &rawEvents[i]
		switch ev.Kind {
		case KindCall:
			if ev.Op != OpRead && ev.Op != OpWrite {
				return nil, nil, 0, fmt.Errorf("call event id=%d has unknown op type", ev.ID)
			}
			if _, exists := unpaired[ev.ID]; exists {
				return nil, nil, 0, fmt.Errorf("duplicate call event id=%d", ev.ID)
			}
			unpaired[ev.ID] = ev
			keyByID[ev.ID] = ev.Key
			events = append(events, porcupine.Event{
				ClientId: ev.ClientID,
				Kind:     porcupine.CallEvent,
				Value:    opInput{Op: ev.Op, Key: ev.Key, Value: ev.Value},
				Id:       ev.ID,
			})
		case KindReturn:
			if _, ok := unpaired[ev.ID]; !ok {
				return nil, nil, 0, fmt.Errorf("return event id=%d has no matching call", ev.ID)
			}
			delete(unpaired, ev.ID)
			// keyByID[ev.ID] was already set when the call event was processed.
			events = append(events, porcupine.Event{
				ClientId: ev.ClientID,
				Kind:     porcupine.ReturnEvent,
				Value:    opOutput{Value: ev.Value, Status: ev.Status},
				Id:       ev.ID,
			})
		default:
			return nil, nil, 0, fmt.Errorf("unknown event kind %d", ev.Kind)
		}
	}

	nUnpaired := len(unpaired)
	for _, ce := range unpaired {
		events = append(events, porcupine.Event{
			ClientId: ce.ClientID,
			Kind:     porcupine.ReturnEvent,
			Value:    opOutput{Status: StatusUnknown},
			Id:       ce.ID,
		})
	}

	return events, keyByID, nUnpaired, nil
}
