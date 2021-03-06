// Copyright 2016 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package activity parses activity manager events in bugreport files and outputs CSV entries for those events.
package activity

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/battery-historian/bugreportutils"
	"github.com/google/battery-historian/csv"
	"github.com/google/battery-historian/historianutils"
	"github.com/google/battery-historian/packageutils"
	usagepb "github.com/google/battery-historian/pb/usagestats_proto"
)

var (
	// logEntryRE is a regular expression that matches the common prefix to event log and logcat lines in the bug report.
	// The details are then matched with the various log event types below.
	// e.g. 11-19 11:29:07.341  2206  2933 I
	logEntryRE = regexp.MustCompile(`^(?P<month>\d+)-(?P<day>\d+)` + `\s+` + `(?P<timeStamp>[^.]+)` + `[.]` + `(?P<remainder>\d+)` + `\s+` + `(?P<pid>\d+)` + `\s+\d+\s+\S+\s+` + `(?P<details>.*)`)

	// activityManagerRE is a regular expression that matches activity manager events.
	activityManagerRE = regexp.MustCompile(`^(?P<transitionType>am_(proc_start|proc_died|low_memory|anr))\s*:` + `\s+` + `\[?(?P<value>[^\]]+)\]?`)

	// bluetoothScanRE is a regular expression that matches bluetooth scan events.
	bluetoothScanRE = regexp.MustCompile(`^.*BluetoothAdapter: startLeScan()`)

	// crashStartRE is a regular expression that matches the first line of a crash event.
	crashStartRE = regexp.MustCompile(`^AndroidRuntime:\s+` + `FATAL\sEXCEPTION:\s+` + `(?P<source>.+)`)

	// crashProcessRE is a regular expression that matches the process information of a crash event.
	crashProcessRE = regexp.MustCompile(`^AndroidRuntime:\s+` + `Process:\s(?P<process>\S+)` + `\s*,\s*` + `PID:\s(?P<pid>.+)`)
)

const (
	// procStartEvent is the string for matching application process start events in the bug report.
	procStartEvent = "am_proc_start"

	// procDiedEvent is the string for matching application process died events in the bug report.
	procDiedEvent = "am_proc_died"

	// anrEvent is the string for matching application not responding events in the bug report.
	anrEvent = "am_anr"

	// lowMemoryEvent is the string for matching low memory events in the bug report.
	lowMemoryEvent = "am_low_memory"

	// lowMemoryANRGroup is the group name for low memory and application not responding events.
	lowMemoryANRGroup = "AM Low Memory / ANR"

	// amProc is the CSV description for the Activity Manager Process related events.
	amProc = "Activity Manager Proc"

	// crashes is the the CSV description of Crash events.
	crashes = "Crashes"
)

// LogsData contains the CSV generated from the system and event logs and the start times of the logs.
type LogsData struct {
	CSV string
	// GroupToLogStart is a map from group name to the earliest start time of the logs the group's events were found in.
	GroupToLogStart map[string]int64
	Warnings        []string
	Errs            []error
}

// procEntry stores the timestamp and details extracted from an am_proc_start or am_proc_died event.
type procEntry struct {
	start     int64
	pid       string
	uid       string
	process   string
	component string
}

// Methods required by csv.EntryState.
func (e *procEntry) GetStartTime() int64 {
	return e.start
}

func (e *procEntry) GetType() string {
	return "service"
}

func (e *procEntry) GetValue() string {
	return fmt.Sprintf("%v~%v~%v~%v", e.pid, e.uid, e.process, e.component)
}

func (e *procEntry) GetKey(desc string) csv.Key {
	return csv.Key{
		desc,
		// The PID is unique while the process is still running.
		e.pid,
	}
}

type parser struct {
	// referenceYear is the year extracted from the dumpstate line in a bugreport. Event log lines don't contain a year in the date string, so we use this to reconstruct the full timestamp.
	referenceYear int

	// referenceMonth is the month extracted from the dumpstate line in a bugreport. Since a bugreport may span over a year boundary, we use the month to check whether the year for the event needs to be decremented or incremented.
	referenceMonth time.Month

	// loc is the location parsed from timezone information in the bugreport. The event log is in the user's local timezone which we need to convert to UTC time.
	loc *time.Location

	// activeProcMap holds the currently active am_proc_start events.
	activeProcMap map[string]*procEntry

	// buf is the buffer to write the CSV events to.
	buf *bytes.Buffer

	// csvState stores and prints out events in CSV format.
	csvState *csv.State

	// pidMappings maps from PID to app info.
	pidMappings map[string][]bugreportutils.AppInfo

	// crashSource stores the last seen crash details.
	crashSource string
}

// newParser creates a parser for the given bugreport.
func newParser(br string) (*parser, []string, error) {
	loc, err := bugreportutils.TimeZone(br)
	if err != nil {
		return nil, []string{}, err
	}
	pm, warnings := bugreportutils.ExtractPIDMappings(br)
	// Extract the year and month from the bugreport dumpstate line.
	d, err := bugreportutils.DumpState(br)
	if err != nil {
		return nil, warnings, fmt.Errorf("could not find dumpstate information in the bugreport: %v", err)
	}
	buf := new(bytes.Buffer)
	return &parser{
		referenceYear:  d.Year(),
		referenceMonth: d.Month(),
		loc:            loc,
		activeProcMap:  make(map[string]*procEntry),
		buf:            buf,
		csvState:       csv.NewState(buf, false),
		pidMappings:    pm,
	}, warnings, nil
}

// fullTimestamp constructs the unix ms timestamp from the given date and time information.
// Since event log events have no corresponding year, we reconstruct the full timestamp using
// the stored reference year and month extracted from the dumpstate line of the bug report.
func (p *parser) fullTimestamp(month, day, partialTimestamp, remainder string) (int64, error) {
	parsedMonth, err := strconv.Atoi(month)
	if err != nil {
		return 0, err
	}
	if !validMonth(parsedMonth) {
		return 0, fmt.Errorf("invalid month: %d", parsedMonth)
	}
	year := p.referenceYear
	// The reference month and year represents the time the bugreport was taken.
	// If the bug report event log begins near the end of a year, and rolls over to the next year,
	// the events will be in either the previous year to the reference year or in the reference year.
	// Bug reports are assumed to span at most a month, but we leave a slightly larger margin here
	// in case we get a slightly longer bug report.
	if p.referenceMonth < time.March && time.Month(parsedMonth) > time.October {
		year--
		// Some events may still occur after the given reference date, so we check for a year rollover.
	} else if p.referenceMonth > time.October && time.Month(parsedMonth) < time.March {
		year++
	}
	return bugreportutils.TimeStampToMs(fmt.Sprintf("%d-%s-%s %s", year, month, day, partialTimestamp), remainder, p.loc)
}

// Parse writes a CSV entry for each line matching activity manager proc start and died, ANR and low memory events.
// Package info is used to match crash events to UIDs. Errors encountered during parsing will be collected into an errors slice and will continue parsing remaining events.
func Parse(pkgs []*usagepb.PackageInfo, f string) LogsData {
	p, warnings, err := newParser(f)
	res := LogsData{Warnings: warnings}
	if err != nil {
		res.Errs = append(res.Errs, err)
		return res
	}
	// Track the first timestamp seen in the current log section.
	var firstSectionTime int64
	// Mapping from group name to the single earliest timestamp seen in all sections the group's events were found in.
	res.GroupToLogStart = make(map[string]int64)

	for _, line := range strings.Split(f, "\n") {
		if m, _ := historianutils.SubexpNames(bugreportutils.BugReportSectionRE, line); m {
			// New section, reset the first seen time.
			firstSectionTime = 0
			continue
		}
		m, result := historianutils.SubexpNames(logEntryRE, line)
		if !m {
			continue
		}
		timestamp, err := p.fullTimestamp(result["month"], result["day"], result["timeStamp"], result["remainder"])
		if err != nil {
			res.Errs = append(res.Errs, err)
			continue
		}
		// Store the first time seen for the current section.
		if firstSectionTime == 0 {
			firstSectionTime = timestamp
		}
		if timestamp < firstSectionTime {
			// Log timestamps should be in sorted order, but still handle the case where they aren't.
			res.Errs = append(res.Errs, fmt.Errorf("expect log timestamps in sorted order, got section start: %v, event timestamp: %v", firstSectionTime, timestamp))
			firstSectionTime = timestamp
		}
		n, warning, err := p.parseEvent(pkgs, timestamp, result["details"], result["pid"])
		if n != "" {
			// Store the section start time if no time was stored for the event.
			// Overwrite the earliest section time for a group if the current section has an earlier start time.
			if t, ok := res.GroupToLogStart[n]; !ok || t > firstSectionTime {
				res.GroupToLogStart[n] = firstSectionTime
			}
		}
		if err != nil {
			res.Errs = append(res.Errs, err)
		}
		if warning != "" {
			res.Warnings = append(res.Warnings, warning)
		}
	}
	// If there was no corresponding am_proc_died event, set the end time to 0.
	p.csvState.PrintAllReset(0)
	res.CSV = p.buf.String()
	return res
}

// parseEvent parses a single event from the log data, and returns the group name the event belongs to and any warning or error.
func (p *parser) parseEvent(pkgs []*usagepb.PackageInfo, timestamp int64, details, pid string) (string, string, error) {
	if strings.Contains(details, "dumpstate: begin") {
		p.csvState.PrintInstantEvent(csv.Entry{
			Desc:  "Logcat misc",
			Start: timestamp,
			Type:  "string",
			Value: "bug report collection triggered",
		})
		return "Logcat misc", "", nil
	}

	if m, _ := historianutils.SubexpNames(bluetoothScanRE, details); m {
		return p.parseBluetoothScan(timestamp, pid), "", nil
	}
	if m, result := historianutils.SubexpNames(crashStartRE, details); m {
		p.crashSource = result["source"]
		return "", "", nil
	}
	if m, result := historianutils.SubexpNames(crashProcessRE, details); m && p.crashSource != "" {
		var uid string
		pkg, err := packageutils.GuessPackage(result["process"], "", pkgs)
		// Still want to show the crash event even if there was an error matching a package, so don't early return.
		if err == nil && pkg != nil {
			uid = strconv.Itoa(int(pkg.GetUid()))
		}
		p.csvState.PrintInstantEvent(csv.Entry{
			Desc:  crashes,
			Start: timestamp,
			Type:  "service",
			Value: fmt.Sprintf("%s: %s", result["process"], p.crashSource),
			Opt:   uid,
		})
		p.crashSource = ""
		return crashes, "", err
	}

	m, result := historianutils.SubexpNames(activityManagerRE, details)
	if !m {
		// Non matching lines are ignored but not considered errors.
		return "", "", nil
	}
	t := result["transitionType"]
	// Format of the value is defined at frameworks/base/services/core/java/com/android/server/am/EventLogTags.logtags.
	v := result["value"]

	switch t {
	case lowMemoryEvent:
		return p.parseLowMemory(timestamp, v), "", nil
	case anrEvent:
		return p.parseANR(pkgs, timestamp, v)
	case procStartEvent, procDiedEvent:
		return p.parseProc(timestamp, v, t)
	default:
		return "", "", fmt.Errorf("unknown transition for %q: %q", amProc, t)
	}
}

func (p *parser) parseBluetoothScan(timestamp int64, pid string) string {
	var appName string
	var uid string
	apps, ok := p.pidMappings[pid]

	if !ok {
		appName = fmt.Sprintf("Unknown PID %s", pid)
	} else {
		// Append the names together in case there's more than one app info.
		var names []string
		for _, app := range apps {
			names = append(names, app.Name)
		}
		sort.Strings(names)
		appName = strings.Join(names, "|")
		// Only use the UID info if there's one mapping.
		if len(apps) == 1 {
			// TODO: consider sharedUserID info.
			uid = apps[0].UID
		}
	}
	p.csvState.PrintInstantEvent(csv.Entry{
		Desc:  "Bluetooth Scan",
		Start: timestamp,
		Type:  "service",
		Value: fmt.Sprintf("%s (PID: %s)", appName, pid),
		Opt:   uid,
	})
	return "Bluetooth Scan"
}

func (p *parser) parseLowMemory(timestamp int64, v string) string {
	// The value is the number of processes.
	p.csvState.PrintInstantEvent(csv.Entry{
		Desc:  "AM Low Memory",
		Start: timestamp,
		Type:  "service",
		Value: v,
	})
	return lowMemoryANRGroup
}

func (p *parser) parseANR(pkgs []*usagepb.PackageInfo, timestamp int64, v string) (string, string, error) {
	// Expected format of v is: User,pid,Package Name,Flags,reason.
	parts := strings.Split(v, ",")
	if len(parts) < 5 {
		return "", "", fmt.Errorf("%s: got %d parts, want 5", anrEvent, len(parts))
	}
	warning := ""
	if len(parts) > 5 {
		warning = fmt.Sprintf("%s: got %d parts, expected 5", anrEvent, len(parts))
	}

	var uid string
	// ANR event should still be displayed even if uid could not be matched.
	// Any error is returned at end of function.
	pkg, err := packageutils.GuessPackage(parts[2], "", pkgs)
	if pkg != nil {
		uid = fmt.Sprintf("%d", pkg.GetUid())
	}
	// We store the UID as part of the ANR value rather than in the Opt field.
	// Usually the Opt field is used to populate a service mapper in the JS, however a less roundabout way is to just have the UID as part of the event itself, which will be specially parsed in the JS code.
	parts = append(parts[1:5], uid)
	p.csvState.PrintInstantEvent(csv.Entry{
		Desc:  "ANR",
		Start: timestamp,
		Type:  "service",
		Value: strings.Join(parts, "~"),
	})
	return lowMemoryANRGroup, warning, err
}

func (p *parser) parseProc(timestamp int64, v string, t string) (string, string, error) {
	e, warning, err := procEvent(timestamp, v, t)
	if err != nil {
		return "", warning, err
	}
	storedEvent, alreadyActive := p.activeProcMap[e.pid]
	switch t {
	case procStartEvent:
		if alreadyActive {
			// Double positive transition. Ignore the event.
			return "", warning, fmt.Errorf("two positive transitions for %q, value %q", amProc, v)
		}
		// Store the new event.
		p.activeProcMap[e.pid] = e
		p.csvState.AddEntryWithOpt(amProc, e, timestamp, e.uid)
		return amProc, warning, nil

	case procDiedEvent:
		if !alreadyActive {
			// No corresponding start event.
			p.csvState.AddEntryWithOpt(amProc, e, 0, e.uid)
			p.csvState.AddEntryWithOpt(amProc, e, timestamp, e.uid)
			return amProc, warning, nil
		}
		// Corresponding start event exists, complete the event with the current timestamp.
		p.csvState.AddEntryWithOpt(amProc, storedEvent, timestamp, storedEvent.uid)
		delete(p.activeProcMap, storedEvent.pid)
		return amProc, warning, nil

	default:
		return "", warning, fmt.Errorf("unknown transition: %v", t)
	}
}

// procEvent returns a procEntry event from the am_proc_start of am_proc_died event.
// If extra fields are encountered, a warning is returned. If fields are missing, an error is returned.
func procEvent(start int64, v string, t string) (*procEntry, string, error) {
	warning := ""
	switch t {
	case procStartEvent:
		// Expected format of v is: User,PID,UID,Process Name,Type,Component.
		parts := strings.Split(v, ",")
		if len(parts) < 6 {
			return nil, warning, fmt.Errorf("%s: got %d parts, want 6", procStartEvent, len(parts))
		}
		if len(parts) > 6 {
			warning = fmt.Sprintf("%s: got %d parts, expected 6", procStartEvent, len(parts))
		}
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return nil, warning, fmt.Errorf("%s: could not parse pid %v: %v", procStartEvent, parts[1], err)
		}
		uid, err := packageutils.AppIDFromString(parts[2])
		if err != nil {
			return nil, warning, fmt.Errorf("%s: could not parse uid %v: %v", procStartEvent, parts[2], err)
		}
		return &procEntry{
			start:     start,
			pid:       parts[1],
			uid:       fmt.Sprint(uid),
			process:   parts[3],
			component: parts[5],
		}, warning, nil

	case procDiedEvent:
		// Expected format of v is: User,PID,Process Name.
		parts := strings.Split(v, ",")
		if len(parts) < 3 {
			return nil, warning, fmt.Errorf("%s: got %d parts, want 3", procDiedEvent, len(parts))
		}
		if len(parts) > 3 {
			warning = fmt.Sprintf("%s: got %d parts, expected 3", procDiedEvent, len(parts))
		}
		if _, err := strconv.Atoi(parts[1]); err != nil {
			return nil, warning, fmt.Errorf("%s: could not parse pid %v: %v", procDiedEvent, parts[1], err)
		}
		return &procEntry{
			start:   start,
			pid:     parts[1],
			process: parts[2],
		}, warning, nil

	default:
		return nil, "", fmt.Errorf("unknown transition: %v", t)
	}
}

func validMonth(m int) bool {
	return m >= int(time.January) && m <= int(time.December)
}
