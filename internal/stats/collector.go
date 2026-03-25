package stats

import (
	"encoding/csv"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/config"
	"github.com/Percona-Lab/percona-load-generator-mongodb/internal/logger"
)

const MaxLatencyBin = 10000

type LatencyHistogram struct {
	mu       sync.Mutex
	Buckets  [MaxLatencyBin]int64
	Overflow int64
	Count    int64
	Sum      float64
	Min      float64
	Max      float64
}

func (h *LatencyHistogram) GetAverage() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.Count == 0 {
		return 0.0
	}
	return h.Sum / float64(h.Count)
}

func (h *LatencyHistogram) Record(ms float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Count++
	h.Sum += ms
	if ms < h.Min {
		h.Min = ms
	}
	if ms > h.Max {
		h.Max = ms
	}
	bucket := int(math.Round(ms))
	if bucket < 0 {
		bucket = 0
	}
	if bucket >= MaxLatencyBin {
		h.Overflow++
	} else {
		h.Buckets[bucket]++
	}
}

func (h *LatencyHistogram) RecordBatch(ms float64, count int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Count += count
	h.Sum += ms * float64(count)
	if ms < h.Min {
		h.Min = ms
	}
	if ms > h.Max {
		h.Max = ms
	}
	bucket := int(math.Round(ms))
	if bucket < 0 {
		bucket = 0
	}
	if bucket >= MaxLatencyBin {
		h.Overflow += count
	} else {
		h.Buckets[bucket] += count
	}
}

func (h *LatencyHistogram) GetPercentile(p float64) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Count == 0 {
		return 0.0
	}
	targetCount := int64(math.Ceil((p / 100.0) * float64(h.Count)))
	var currentCount int64 = 0
	for i, count := range h.Buckets {
		currentCount += count
		if currentCount >= targetCount {
			return float64(i)
		}
	}
	return float64(MaxLatencyBin)
}

func (h *LatencyHistogram) GetStats() map[string]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.Count == 0 {
		return map[string]float64{"avg": 0, "min": 0, "max": 0, "p95": 0, "p99": 0, "sum": 0, "count": 0}
	}

	min := h.Min
	if min == math.MaxFloat64 {
		min = 0 // Sanity check to prevent rendering giant float limits
	}

	// Inline percentile calculation to avoid deadlocking
	getPerc := func(p float64) float64 {
		targetCount := int64(math.Ceil((p / 100.0) * float64(h.Count)))
		var currentCount int64 = 0
		for i, count := range h.Buckets {
			currentCount += count
			if currentCount >= targetCount {
				return float64(i)
			}
		}
		return float64(MaxLatencyBin)
	}

	return map[string]float64{
		"avg":   h.Sum / float64(h.Count),
		"min":   min,
		"max":   h.Max,
		"p95":   getPerc(95.0),
		"p99":   getPerc(99.0),
		"sum":   h.Sum,
		"count": float64(h.Count),
	}
}

func (h *LatencyHistogram) GetStatsAndReset() map[string]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.Count == 0 {
		return map[string]float64{"avg": 0, "min": 0, "max": 0, "p95": 0, "p99": 0, "sum": 0, "count": 0}
	}

	min := h.Min
	if min == math.MaxFloat64 {
		min = 0
	}

	getPerc := func(p float64) float64 {
		targetCount := int64(math.Ceil((p / 100.0) * float64(h.Count)))
		var currentCount int64 = 0
		for i, count := range h.Buckets {
			currentCount += count
			if currentCount >= targetCount {
				return float64(i)
			}
		}
		return float64(MaxLatencyBin)
	}

	stats := map[string]float64{
		"avg":   h.Sum / float64(h.Count),
		"min":   min,
		"max":   h.Max,
		"p95":   getPerc(95.0),
		"p99":   getPerc(99.0),
		"sum":   h.Sum,
		"count": float64(h.Count),
	}

	// Reset the histogram for the next interval
	h.Buckets = [MaxLatencyBin]int64{}
	h.Overflow = 0
	h.Count = 0
	h.Sum = 0
	h.Min = math.MaxFloat64
	h.Max = 0

	return stats
}

type Collector struct {
	FindOps   uint64
	InsertOps uint64
	UpsertOps uint64
	UpdateOps uint64
	DeleteOps uint64
	AggOps    uint64
	TransOps  uint64

	FindHist           *LatencyHistogram
	InsertHist         *LatencyHistogram
	UpsertHist         *LatencyHistogram
	UpdateHist         *LatencyHistogram
	DeleteHist         *LatencyHistogram
	AggHist            *LatencyHistogram
	TransHist          *LatencyHistogram
	TotalHist          *LatencyHistogram
	IntervalFindHist   *LatencyHistogram
	IntervalInsertHist *LatencyHistogram
	IntervalUpsertHist *LatencyHistogram
	IntervalUpdateHist *LatencyHistogram
	IntervalDeleteHist *LatencyHistogram
	IntervalAggHist    *LatencyHistogram
	IntervalTransHist  *LatencyHistogram
	IntervalTotalHist  *LatencyHistogram

	CurrentIteration int

	startTime  time.Time
	prevFind   uint64
	prevInsert uint64
	prevUpsert uint64
	prevUpdate uint64
	prevDelete uint64
	prevAgg    uint64
	prevTrans  uint64
}

func NewCollector() *Collector {
	return &Collector{
		FindHist:           &LatencyHistogram{Min: math.MaxFloat64},
		InsertHist:         &LatencyHistogram{Min: math.MaxFloat64},
		UpsertHist:         &LatencyHistogram{Min: math.MaxFloat64},
		UpdateHist:         &LatencyHistogram{Min: math.MaxFloat64},
		DeleteHist:         &LatencyHistogram{Min: math.MaxFloat64},
		AggHist:            &LatencyHistogram{Min: math.MaxFloat64},
		TransHist:          &LatencyHistogram{Min: math.MaxFloat64},
		TotalHist:          &LatencyHistogram{Min: math.MaxFloat64},
		IntervalFindHist:   &LatencyHistogram{Min: math.MaxFloat64},
		IntervalInsertHist: &LatencyHistogram{Min: math.MaxFloat64},
		IntervalUpsertHist: &LatencyHistogram{Min: math.MaxFloat64},
		IntervalUpdateHist: &LatencyHistogram{Min: math.MaxFloat64},
		IntervalDeleteHist: &LatencyHistogram{Min: math.MaxFloat64},
		IntervalAggHist:    &LatencyHistogram{Min: math.MaxFloat64},
		IntervalTransHist:  &LatencyHistogram{Min: math.MaxFloat64},
		IntervalTotalHist:  &LatencyHistogram{Min: math.MaxFloat64},
		startTime:          time.Now(),
	}
}

func (c *Collector) Track(opType string, duration time.Duration) {
	ms := float64(duration.Nanoseconds()) / 1e6
	c.TotalHist.Record(ms)
	c.IntervalTotalHist.Record(ms)
	switch opType {
	case "find":
		atomic.AddUint64(&c.FindOps, 1)
		c.FindHist.Record(ms)
		c.IntervalFindHist.Record(ms)
	case "insert":
		atomic.AddUint64(&c.InsertOps, 1)
		c.InsertHist.Record(ms)
		c.IntervalInsertHist.Record(ms)
	case "upsert":
		atomic.AddUint64(&c.UpsertOps, 1)
		c.UpsertHist.Record(ms)
		c.IntervalUpsertHist.Record(ms)
	case "update", "updateOne", "updateMany":
		atomic.AddUint64(&c.UpdateOps, 1)
		c.UpdateHist.Record(ms)
		c.IntervalUpdateHist.Record(ms)
	case "delete", "deleteOne", "deleteMany":
		atomic.AddUint64(&c.DeleteOps, 1)
		c.DeleteHist.Record(ms)
		c.IntervalDeleteHist.Record(ms)
	case "aggregate":
		atomic.AddUint64(&c.AggOps, 1)
		c.AggHist.Record(ms)
		c.IntervalAggHist.Record(ms)
	case "transaction":
		atomic.AddUint64(&c.TransOps, 1)
		c.TransHist.Record(ms)
		c.IntervalTransHist.Record(ms)
	}
}

func (c *Collector) Add(opType string, count int64, duration time.Duration) {
	ms := float64(duration.Nanoseconds()) / 1e6
	c.TotalHist.RecordBatch(ms, count)
	c.IntervalTotalHist.RecordBatch(ms, count)
	switch opType {
	case "find":
		atomic.AddUint64(&c.FindOps, uint64(count))
		c.FindHist.RecordBatch(ms, count)
		c.IntervalFindHist.RecordBatch(ms, count)
	case "insert":
		atomic.AddUint64(&c.InsertOps, uint64(count))
		c.InsertHist.RecordBatch(ms, count)
		c.IntervalInsertHist.RecordBatch(ms, count)
	case "upsert":
		atomic.AddUint64(&c.UpsertOps, uint64(count))
		c.UpsertHist.RecordBatch(ms, count)
		c.IntervalUpsertHist.RecordBatch(ms, count)
	case "update", "updateOne", "updateMany":
		atomic.AddUint64(&c.UpdateOps, uint64(count))
		c.UpdateHist.RecordBatch(ms, count)
		c.IntervalUpdateHist.RecordBatch(ms, count)
	case "delete", "deleteOne", "deleteMany":
		atomic.AddUint64(&c.DeleteOps, uint64(count))
		c.DeleteHist.RecordBatch(ms, count)
		c.IntervalDeleteHist.RecordBatch(ms, count)
	case "aggregate":
		atomic.AddUint64(&c.AggOps, uint64(count))
		c.AggHist.RecordBatch(ms, count)
		c.IntervalAggHist.RecordBatch(ms, count)
	case "transaction":
		atomic.AddUint64(&c.TransOps, uint64(count))
		c.TransHist.RecordBatch(ms, count)
		c.IntervalTransHist.RecordBatch(ms, count)
	}
}

const monitorLayout = " %-7s | %9s | %7s | %7s | %7s | %7s | %7s | %6s | %5s\n"

func (c *Collector) Monitor(done <-chan struct{}, refreshRateSec int, concurrency int, csvEnabled bool, csvAppend bool, csvPath string, silent ...bool) {
	isSilent := false
	if len(silent) > 0 {
		isSilent = silent[0]
	}

	ticker := time.NewTicker(time.Duration(refreshRateSec) * time.Second)
	defer ticker.Stop()

	// --- CSV EXPORT SETUP ---
	var csvFile *os.File
	var csvWriter *csv.Writer
	if csvEnabled {
		var err error
		needsHeader := true

		if csvAppend {
			// Smart Header Check: If the file exists and has data, skip the header
			if info, e := os.Stat(csvPath); e == nil && info.Size() > 0 {
				needsHeader = false
			}
			// Open in Append mode
			csvFile, err = os.OpenFile(csvPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		} else {
			// Standard overwrite mode
			csvFile, err = os.Create(csvPath)
		}

		if err != nil {
			fmt.Println(logger.RedString(fmt.Sprintf("> Warning: Failed to open CSV export file: %v", err)))
		} else {
			defer csvFile.Close()
			csvWriter = csv.NewWriter(csvFile)

			if needsHeader {
				csvWriter.Write([]string{
					"Timestamp", "ElapsedSec",
					"Select_OpsSec", "Insert_OpsSec", "Upsert_OpsSec", "Update_OpsSec", "Delete_OpsSec", "Agg_OpsSec", "Trans_OpsSec",
					"Total_Lat_Avg", "Total_Lat_P99",
					"Select_Lat_Avg", "Select_Lat_P99",
					"Insert_Lat_Avg", "Insert_Lat_P99",
					"Upsert_Lat_Avg", "Upsert_Lat_P99",
					"Update_Lat_Avg", "Update_Lat_P99",
					"Delete_Lat_Avg", "Delete_Lat_P99",
					"Agg_Lat_Avg", "Agg_Lat_P99",
					"Trans_Lat_Avg", "Trans_Lat_P99",
					"Iteration",
				})
				csvWriter.Flush()
			}

		}
	}

	if !isSilent {
		fmt.Println()
		fmt.Println(logger.GreenString("> Starting Workload..."))
		header := fmt.Sprintf(monitorLayout, "TIME", "TOTAL OPS", "SELECT", "INSERT", "UPSERT", "UPDATE", "DELETE", "AGG", "TRANS")
		fmt.Print(logger.BoldString(header))
		fmt.Println(logger.CyanString(" -----------------------------------------------------------------------------------------"))
	} else {
		fmt.Println()
		fmt.Println(logger.CyanString("[Web UI Active] Workload started. CLI output is disabled. Please view live progress in the browser dashboard."))
	}

	startTime := time.Now()

	// Initialize trackers with CURRENT values so we don't carry over previous iterations as a massive spike
	lastFind := atomic.LoadUint64(&c.FindOps)
	lastInsert := atomic.LoadUint64(&c.InsertOps)
	lastUpsert := atomic.LoadUint64(&c.UpsertOps)
	lastUpdate := atomic.LoadUint64(&c.UpdateOps)
	lastDelete := atomic.LoadUint64(&c.DeleteOps)
	lastAgg := atomic.LoadUint64(&c.AggOps)
	lastTrans := atomic.LoadUint64(&c.TransOps)

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !isSilent {
				c.printInterval()
			}

			// --- CSV ROW WRITER ---
			if csvWriter != nil {
				elapsed := time.Since(startTime).Seconds()

				// Load current totals safely using the correct struct field names
				currentFind := atomic.LoadUint64(&c.FindOps)
				currentInsert := atomic.LoadUint64(&c.InsertOps)
				currentUpsert := atomic.LoadUint64(&c.UpsertOps)
				currentUpdate := atomic.LoadUint64(&c.UpdateOps)
				currentDelete := atomic.LoadUint64(&c.DeleteOps)
				currentAgg := atomic.LoadUint64(&c.AggOps)
				currentTrans := atomic.LoadUint64(&c.TransOps)

				// Calculate Ops/Sec for this specific window
				rateFind := float64(currentFind-lastFind) / float64(refreshRateSec)
				rateInsert := float64(currentInsert-lastInsert) / float64(refreshRateSec)
				rateUpsert := float64(currentUpsert-lastUpsert) / float64(refreshRateSec)
				rateUpdate := float64(currentUpdate-lastUpdate) / float64(refreshRateSec)
				rateDelete := float64(currentDelete-lastDelete) / float64(refreshRateSec)
				rateAgg := float64(currentAgg-lastAgg) / float64(refreshRateSec)
				rateTrans := float64(currentTrans-lastTrans) / float64(refreshRateSec)

				// Fetch cumulative latency stats at this exact point in time
				latStats := c.IntervalTotalHist.GetStatsAndReset()
				totLat := c.IntervalTotalHist.GetStatsAndReset()
				fndLat := c.IntervalFindHist.GetStatsAndReset()
				insLat := c.IntervalInsertHist.GetStatsAndReset()
				upsLat := c.IntervalUpsertHist.GetStatsAndReset()
				updLat := c.IntervalUpdateHist.GetStatsAndReset()
				delLat := c.IntervalDeleteHist.GetStatsAndReset()
				aggLat := c.IntervalAggHist.GetStatsAndReset()
				trnLat := c.IntervalTransHist.GetStatsAndReset()

				iter := c.CurrentIteration
				if iter < 1 {
					iter = 1
				}

				csvWriter.Write([]string{
					time.Now().Format(time.RFC3339),
					fmt.Sprintf("%.0f", elapsed),
					fmt.Sprintf("%.2f", rateFind),
					fmt.Sprintf("%.2f", rateInsert),
					fmt.Sprintf("%.2f", rateUpsert),
					fmt.Sprintf("%.2f", rateUpdate),
					fmt.Sprintf("%.2f", rateDelete),
					fmt.Sprintf("%.2f", rateAgg),
					fmt.Sprintf("%.2f", rateTrans),
					fmt.Sprintf("%.2f", latStats["avg"]),
					fmt.Sprintf("%.2f", latStats["min"]),
					fmt.Sprintf("%.2f", latStats["max"]),
					fmt.Sprintf("%.2f", latStats["p95"]),
					fmt.Sprintf("%.2f", latStats["p99"]),
					fmt.Sprintf("%.2f", totLat["avg"]), fmt.Sprintf("%.2f", totLat["p99"]),
					fmt.Sprintf("%.2f", fndLat["avg"]), fmt.Sprintf("%.2f", fndLat["p99"]),
					fmt.Sprintf("%.2f", insLat["avg"]), fmt.Sprintf("%.2f", insLat["p99"]),
					fmt.Sprintf("%.2f", upsLat["avg"]), fmt.Sprintf("%.2f", upsLat["p99"]),
					fmt.Sprintf("%.2f", updLat["avg"]), fmt.Sprintf("%.2f", updLat["p99"]),
					fmt.Sprintf("%.2f", delLat["avg"]), fmt.Sprintf("%.2f", delLat["p99"]),
					fmt.Sprintf("%.2f", aggLat["avg"]), fmt.Sprintf("%.2f", aggLat["p99"]),
					fmt.Sprintf("%.2f", trnLat["avg"]), fmt.Sprintf("%.2f", trnLat["p99"]),
					strconv.Itoa(iter),
				})
				csvWriter.Flush()

				// Update 'last' trackers for the next tick
				lastFind = currentFind
				lastInsert = currentInsert
				lastUpsert = currentUpsert
				lastUpdate = currentUpdate
				lastDelete = currentDelete
				lastAgg = currentAgg
				lastTrans = currentTrans
			}
		}
	}
}

func (c *Collector) printInterval() {
	cF := atomic.LoadUint64(&c.FindOps)
	cI := atomic.LoadUint64(&c.InsertOps)
	cUP := atomic.LoadUint64(&c.UpsertOps)
	cU := atomic.LoadUint64(&c.UpdateOps)
	cD := atomic.LoadUint64(&c.DeleteOps)
	cA := atomic.LoadUint64(&c.AggOps)
	cT := atomic.LoadUint64(&c.TransOps)

	dF := cF - c.prevFind
	dI := cI - c.prevInsert
	dUP := cUP - c.prevUpsert
	dU := cU - c.prevUpdate
	dD := cD - c.prevDelete
	dA := cA - c.prevAgg
	dT := cT - c.prevTrans

	c.prevFind, c.prevInsert, c.prevUpsert, c.prevUpdate = cF, cI, cUP, cU
	c.prevDelete, c.prevAgg, c.prevTrans = cD, cA, cT

	totalDelta := dF + dI + dUP + dU + dD + dA + dT

	elapsed := time.Since(c.startTime).Truncate(time.Second)
	elapsedStr := fmt.Sprintf("%02d:%02d", int(elapsed.Minutes()), int(elapsed.Seconds())%60)

	totalOpsFormatted := logger.BoldString(fmt.Sprintf("%9s", formatInt(int64(totalDelta))))

	fmt.Printf(monitorLayout,
		elapsedStr,
		totalOpsFormatted,
		formatInt(int64(dF)),
		formatInt(int64(dI)),
		formatInt(int64(dUP)),
		formatInt(int64(dU)),
		formatInt(int64(dD)),
		formatInt(int64(dA)),
		formatInt(int64(dT)),
	)
}

func (c *Collector) PrintFinalSummary(duration time.Duration, silent ...bool) {
	isSilent := false
	if len(silent) > 0 {
		isSilent = silent[0]
	}
	// If silent is true, return immediately without printing to CLI
	if isSilent {
		fmt.Printf("\n%s\n\n", logger.CyanString("[Web UI Active] Workload finished (Duration: %.2fs). Final summary is available in the browser dashboard.", duration.Seconds()))
		return
	}
	fO, iO, upO, uO, dO, aO, tO := atomic.LoadUint64(&c.FindOps), atomic.LoadUint64(&c.InsertOps), atomic.LoadUint64(&c.UpsertOps), atomic.LoadUint64(&c.UpdateOps), atomic.LoadUint64(&c.DeleteOps), atomic.LoadUint64(&c.AggOps), atomic.LoadUint64(&c.TransOps)
	totalOps := fO + iO + upO + uO + dO + aO + tO
	seconds := duration.Seconds()

	fmt.Println()
	fmt.Println(logger.GreenString("> Workload Finished."))
	fmt.Println()
	fmt.Println(logger.BoldString("  SUMMARY"))
	fmt.Println(logger.CyanString("  --------------------------------------------------"))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  Runtime:\t%.2fs\n", seconds)
	fmt.Fprintf(w, "  Total Ops:\t%s\n", formatInt(int64(totalOps)))
	avgRate := 0.0
	if seconds > 0 {
		avgRate = float64(totalOps) / seconds
	}
	fmt.Fprintf(w, "  Avg Rate:\t%s ops/sec\n", logger.BoldString(formatInt(int64(avgRate))))
	w.Flush()

	fmt.Println()
	fmt.Println(logger.BoldString("  LATENCY DISTRIBUTION (ms)"))
	fmt.Println(logger.CyanString("  --------------------------------------------------"))
	const layout = "  %-7s   %10s   %10s   %10s   %10s   %10s"
	fmt.Println(logger.BoldString(fmt.Sprintf(layout, "TYPE", "AVG", "MIN", "MAX", "P95", "P99")))
	printLatencyRow(layout, "SELECT", c.FindHist)
	printLatencyRow(layout, "INSERT", c.InsertHist)
	printLatencyRow(layout, "UPSERT", c.UpsertHist)
	printLatencyRow(layout, "UPDATE", c.UpdateHist)
	printLatencyRow(layout, "DELETE", c.DeleteHist)
	printLatencyRow(layout, "AGG", c.AggHist)
	printLatencyRow(layout, "TRANS", c.TransHist)
	fmt.Println()
}

func printLatencyRow(layout string, label string, h *LatencyHistogram) {
	if h.Count == 0 {
		fmt.Printf(layout+"\n", label, "-", "-", "-", "-", "-")
		return
	}
	avgMs := h.Sum / float64(h.Count)
	fmt.Printf(layout+"\n", label, formatLatency(avgMs), formatLatency(h.Min), formatLatency(h.Max), formatLatency(h.GetPercentile(95.0)), formatLatency(h.GetPercentile(99.0)))
}

func formatLatency(ms float64) string {
	if ms < 1000.0 {
		return fmt.Sprintf("%.2f ms", ms)
	}
	if ms < 60000.0 {
		return fmt.Sprintf("%.4f s", ms/1000.0)
	}
	return fmt.Sprintf("%.2f m", ms/60000.0)
}

func formatInt(n int64) string {
	in := strconv.FormatInt(n, 10)
	numOfDigits := len(in)
	if n < 0 {
		numOfDigits--
	}
	numOfCommas := (numOfDigits - 1) / 3
	out := make([]byte, len(in)+numOfCommas)
	if n < 0 {
		in, out[0] = in[1:], '-'
	}
	for i, j, k := len(in)-1, len(out)-1, 0; ; i, j = i-1, j-1 {
		out[j] = in[i]
		if i == 0 {
			return string(out)
		}
		if k++; k == 3 {
			j, k = j-1, 0
			out[j] = ','
		}
	}
}

// Helper to get active env vars dynamically
func getOverriddenEnvVars() []string {
	var overrides []string

	// Dynamic: Iterate over all environment variables
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "PLGM_") {
			parts := strings.SplitN(env, "=", 2)
			key := parts[0]

			// Filter out Password entirely
			if key == "PLGM_PASSWORD" {
				continue
			}

			overrides = append(overrides, env)
		}
	}
	sort.Strings(overrides)
	return overrides
}

func PrintConfiguration(appCfg *config.AppConfig, collections []config.CollectionDefinition, version string) {
	fmt.Println()
	fmt.Printf("  %s\n", logger.CyanString("plgm %s", version))
	fmt.Println(logger.CyanString("  --------------------------------------------------"))
	safeURI := appCfg.URI
	u, err := url.Parse(appCfg.URI)
	if err == nil && u.User != nil {
		if p, hasPassword := u.User.Password(); hasPassword {
			safeURI = strings.Replace(appCfg.URI, p, "xxxxxx", 1)
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  Target URI:\t%s\n", safeURI)

	var namespaces []string
	for _, col := range collections {
		namespaces = append(namespaces, fmt.Sprintf("%s.%s", col.DatabaseName, col.Name))
	}
	fmt.Fprintf(w, "  Namespaces:\t%s\n", strings.Join(namespaces, ", "))
	fmt.Fprintf(w, "  Workers:\t%d active\n", appCfg.Concurrency)
	fmt.Fprintf(w, "  Duration:\t%s\n", appCfg.Duration)

	isSingleFile := false
	if appCfg.CollectionsPath != "" {
		info, err := os.Stat(appCfg.CollectionsPath)
		if err == nil && !info.IsDir() {
			isSingleFile = true
		}
	}

	mode := "Custom (default.json excluded)"

	if isSingleFile {
		mode = "Explicit File (Default filtering ignored)"
	} else {
		// Directory or Empty Path Logic
		if appCfg.DefaultWorkload {
			mode = "Default (Only default.json)"
			// Warning: Only show if user explicitly set PLGM_COLLECTIONS_PATH in env
			// This avoids warning users who just run with default config.yaml
			if os.Getenv("PLGM_COLLECTIONS_PATH") != "" {
				mode += " [Warning: Ignoring other files in custom path!]"
			}
		} else {
			if appCfg.CollectionsPath == "" {
				mode = "Custom (No path provided?)"
			}
		}
	}
	fmt.Fprintf(w, "  Workload Mode:\t%s\n", mode)

	w.Flush()

	overrides := getOverriddenEnvVars()
	if len(overrides) > 0 {
		fmt.Println()
		fmt.Println(logger.BoldString("  ACTIVE OVERRIDES (Env)"))
		for _, o := range overrides {
			fmt.Printf("   -> %s\n", o)
		}
	}

	fmt.Println()
	fmt.Println(logger.BoldString("  WORKLOAD DEFINITION"))
	fmt.Println(logger.CyanString("  --------------------------------------------------"))
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  Distribution:\tSelect (%d%%)\tUpdate (%d%%)\n", appCfg.FindPercent, appCfg.UpdatePercent)
	fmt.Fprintf(w, "  \tInsert (%d%%)\tDelete (%d%%)\n", appCfg.InsertPercent, appCfg.DeletePercent)
	fmt.Fprintf(w, "  \tAgg    (%d%%)\tTrans  (%d%%)\n", appCfg.AggregatePercent, appCfg.TransactionPercent)
	w.Flush()
	fmt.Println()
}
