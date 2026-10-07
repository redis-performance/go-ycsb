package measurement

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pingcap/go-ycsb/pkg/prop"
)

func (h *histograms) openHDRMinutes(path string) error {
	for _, other := range []string{
		h.p.GetString(prop.MeasurementRawOutputFile, ""),
		h.p.GetString(prop.MeasurementIntervalOutputFile, ""),
	} {
		if other != "" && filepath.Clean(path) == filepath.Clean(other) {
			return fmt.Errorf("%s conflicts with another measurement output file (%s)", prop.MeasurementHDRMinuteOutputFile, path)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("HDR minute output file: %w", err)
	}
	h.iv.hdrMinuteFile = f
	h.iv.hdrMinuteOut = bufio.NewWriter(f)
	h.iv.hdrMinuteCh = make(chan []HDRWindowRecord, 4)
	h.iv.hdrMinuteDone = make(chan struct{})
	go h.writeHDRMinutes(h.iv.hdrMinuteCh, h.iv.hdrMinuteDone, h.iv.hdrMinuteOut)
	return nil
}

// writeHDRMinuteLocked queues the completed 60-slice distribution at each
// minute boundary. Disk writes happen on a separate goroutine, so a slow
// output file cannot hold up the interval reporter.
func (h *histograms) writeHDRMinuteLocked(records []HDRWindowRecord, end time.Time) {
	if h.iv.hdrMinuteCh == nil {
		return
	}
	if end.Sub(h.iv.hdrMinuteLast) < time.Minute || h.iv.hdrMinuteErr != nil {
		return
	}
	h.iv.hdrMinuteLast = end
	minuteRecords := make([]HDRWindowRecord, 0, len(records)/2)
	for i := range records {
		if records[i].WindowSeconds == packedWindowSlots {
			minuteRecords = append(minuteRecords, records[i])
		}
	}
	select {
	case h.iv.hdrMinuteCh <- h.hdrWithBuckets(minuteRecords):
	default:
		h.iv.hdrMinuteErr = fmt.Errorf("minute writer queue is full")
	}
}

func (h *histograms) writeHDRMinutes(ch <-chan []HDRWindowRecord, done chan<- struct{}, out *bufio.Writer) {
	defer close(done)
	enc := json.NewEncoder(out)
	writeFailed := false
	for records := range ch {
		if writeFailed {
			continue
		}
		var err error
		for i := range records {
			if err = enc.Encode(&records[i]); err != nil {
				break
			}
		}
		if err == nil {
			err = out.Flush()
		}
		if err != nil {
			writeFailed = true
			h.iv.mu.Lock()
			if h.iv.hdrMinuteErr == nil {
				h.iv.hdrMinuteErr = err
				fmt.Fprintf(os.Stderr, "HDR minute output: %v (no more minute records)\n", err)
			}
			h.iv.mu.Unlock()
		}
	}
}

func (h *histograms) closeHDRMinutes() error {
	h.iv.mu.Lock()
	if h.iv.hdrMinuteCh != nil {
		close(h.iv.hdrMinuteCh)
		h.iv.hdrMinuteCh = nil
	}
	done := h.iv.hdrMinuteDone
	h.iv.mu.Unlock()
	if done != nil {
		<-done
	}
	h.iv.mu.Lock()
	defer h.iv.mu.Unlock()
	if h.iv.hdrMinuteFile == nil {
		return h.iv.hdrMinuteErr
	}
	if h.iv.hdrMinuteErr == nil {
		h.iv.hdrMinuteErr = h.iv.hdrMinuteOut.Flush()
	}
	if err := h.iv.hdrMinuteFile.Close(); h.iv.hdrMinuteErr == nil {
		h.iv.hdrMinuteErr = err
	}
	h.iv.hdrMinuteFile, h.iv.hdrMinuteOut = nil, nil
	if h.iv.hdrMinuteErr != nil {
		return fmt.Errorf("HDR minute output file: %w", h.iv.hdrMinuteErr)
	}
	return nil
}
