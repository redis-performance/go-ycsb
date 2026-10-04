// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package measurement

import (
	"bufio"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/util"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
)

// intervalMeasurer is a Measurer that also reports per interval (histogram).
type intervalMeasurer interface {
	IntervalStart(now time.Time)
	IntervalTick(now time.Time)
	IntervalClose(now time.Time) error
}

var header = []string{"Operation", "Takes(s)", "Count", "OPS", "Avg(us)", "Min(us)", "Max(us)", "50th(us)", "90th(us)", "95th(us)", "99th(us)", "99.9th(us)", "99.99th(us)"}

type measureEvent struct {
	op    string
	start time.Time
	lan   time.Duration
}

type measurement struct {
	sync.RWMutex

	p *properties.Properties

	measurer ycsb.Measurer
	interval time.Duration
}

var measureChan chan measureEvent
var measureWg sync.WaitGroup
var measureOnce sync.Once

func (m *measurement) measure(op string, start time.Time, lan time.Duration) {
	m.measurer.Measure(op, start, lan)
}

func (m *measurement) output() {
	m.RLock()
	defer m.RUnlock()

	outFile := m.p.GetString(prop.MeasurementRawOutputFile, "")
	var w *bufio.Writer
	if outFile == "" {
		w = bufio.NewWriter(os.Stdout)
	} else {
		f, err := os.Create(outFile)
		if err != nil {
			panic("failed to create output file: " + err.Error())
		}
		defer f.Close()
		w = bufio.NewWriter(f)
	}

	err := globalMeasure.measurer.Output(w)
	if err != nil {
		panic("failed to write output: " + err.Error())
	}

	err = w.Flush()
	if err != nil {
		panic("failed to flush output: " + err.Error())
	}
}

func (m *measurement) summary() {
	m.RLock()
	globalMeasure.measurer.Summary()
	m.RUnlock()
}

// InitMeasure initializes the global measurement.
func InitMeasure(p *properties.Properties) {
	globalMeasure = new(measurement)
	globalMeasure.p = p
	measurementType := p.GetString(prop.MeasurementType, prop.MeasurementTypeDefault)
	interval, err := ParseInterval(p)
	if err != nil {
		util.Fatalf("%v", err)
	}
	globalMeasure.interval = interval
	intervalFile := p.GetString(prop.MeasurementIntervalOutputFile, "")
	switch measurementType {
	case "histogram":
		h := InitHistograms(p)
		if intervalFile != "" {
			if err := h.openIntervals(intervalFile); err != nil {
				util.Fatalf("%v", err)
			}
		}
		globalMeasure.measurer = h
	case "raw", "csv":
		if intervalFile != "" {
			util.Fatalf("%s needs %s=histogram", prop.MeasurementIntervalOutputFile, prop.MeasurementType)
		}
		globalMeasure.measurer = InitCSV()
	default:
		panic("unsupported measurement type: " + measurementType)
	}
	EnableWarmUp(p.GetInt64(prop.WarmUpTime, 0) > 0)

	measureChan = make(chan measureEvent, 1000000) // tune size if needed
	measureWg.Add(1)
	go func() {
		defer measureWg.Done()
		for ev := range measureChan {
			globalMeasure.measure(ev.op, ev.start, ev.lan)
		}
	}()
}

// ReportInterval is the reporting interval (prop.LogInterval).
func ReportInterval() time.Duration {
	return globalMeasure.interval
}

// StartIntervals starts the reporting intervals; call it when warm-up ends.
func StartIntervals() {
	if im, ok := globalMeasure.measurer.(intervalMeasurer); ok {
		im.IntervalStart(time.Now())
	}
}

// IntervalTick ends a reporting interval (after the status lines).
func IntervalTick() {
	if im, ok := globalMeasure.measurer.(intervalMeasurer); ok {
		im.IntervalTick(time.Now())
	}
}

// Output prints the complete measurements. The error is the interval output
// file's: the summary is printed regardless, and the caller decides how to fail.
func Output() error {
	measureOnce.Do(func() {
		close(measureChan)
		measureWg.Wait()
	})
	// every sample is recorded now: the last, partial interval is complete
	var err error
	if im, ok := globalMeasure.measurer.(intervalMeasurer); ok {
		err = im.IntervalClose(time.Now())
	}
	globalMeasure.measurer.GenerateExtendedOutputs()
	globalMeasure.output()
	return err
}

// Summary prints the measurement summary.
func Summary() {
	globalMeasure.summary()
}

// EnableWarmUp sets whether to enable warm-up.
func EnableWarmUp(b bool) {
	if b {
		atomic.StoreInt32(&warmUp, 1)
	} else {
		atomic.StoreInt32(&warmUp, 0)
	}
}

// IsWarmUpFinished returns whether warm-up is finished or not.
func IsWarmUpFinished() bool {
	return atomic.LoadInt32(&warmUp) == 0
}

// Measure measures the operation.
func Measure(op string, start time.Time, lan time.Duration) {
	if IsWarmUpFinished() {
		// Retry until we can send to the channel
		measureChan <- measureEvent{op, start, lan}
	}
}

var globalMeasure *measurement
var warmUp int32 // use as bool, 1 means in warmup progress, 0 means warmup finished.
