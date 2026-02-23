package stats

import (
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

type Collector struct {
	FindOps   uint64
	InsertOps uint64
	UpsertOps uint64
	UpdateOps uint64
	DeleteOps uint64
	AggOps    uint64
	TransOps  uint64

	FindHist   *LatencyHistogram
	InsertHist *LatencyHistogram
	UpsertHist *LatencyHistogram
	UpdateHist *LatencyHistogram
	DeleteHist *LatencyHistogram
	AggHist    *LatencyHistogram
	TransHist  *LatencyHistogram

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
		FindHist:   &LatencyHistogram{Min: math.MaxFloat64},
		InsertHist: &LatencyHistogram{Min: math.MaxFloat64},
		UpsertHist: &LatencyHistogram{Min: math.MaxFloat64},
		UpdateHist: &LatencyHistogram{Min: math.MaxFloat64},
		DeleteHist: &LatencyHistogram{Min: math.MaxFloat64},
		AggHist:    &LatencyHistogram{Min: math.MaxFloat64},
		TransHist:  &LatencyHistogram{Min: math.MaxFloat64},
		startTime:  time.Now(),
	}
}

func (c *Collector) Track(opType string, duration time.Duration) {
	ms := float64(duration.Nanoseconds()) / 1e6
	switch opType {
	case "find":
		atomic.AddUint64(&c.FindOps, 1)
		c.FindHist.Record(ms)
	case "insert":
		atomic.AddUint64(&c.InsertOps, 1)
		c.InsertHist.Record(ms)
	case "upsert":
		atomic.AddUint64(&c.UpsertOps, 1)
		c.UpsertHist.Record(ms)
	case "update", "updateOne", "updateMany":
		atomic.AddUint64(&c.UpdateOps, 1)
		c.UpdateHist.Record(ms)
	case "delete", "deleteOne", "deleteMany":
		atomic.AddUint64(&c.DeleteOps, 1)
		c.DeleteHist.Record(ms)
	case "aggregate":
		atomic.AddUint64(&c.AggOps, 1)
		c.AggHist.Record(ms)
	case "transaction":
		atomic.AddUint64(&c.TransOps, 1)
		c.TransHist.Record(ms)
	}
}

func (c *Collector) Add(opType string, count int64, duration time.Duration) {
	ms := float64(duration.Nanoseconds()) / 1e6
	switch opType {
	case "find":
		atomic.AddUint64(&c.FindOps, uint64(count))
		c.FindHist.RecordBatch(ms, count)
	case "insert":
		atomic.AddUint64(&c.InsertOps, uint64(count))
		c.InsertHist.RecordBatch(ms, count)
	case "upsert":
		atomic.AddUint64(&c.UpsertOps, uint64(count))
		c.UpsertHist.RecordBatch(ms, count)
	case "update", "updateOne", "updateMany":
		atomic.AddUint64(&c.UpdateOps, uint64(count))
		c.UpdateHist.RecordBatch(ms, count)
	case "delete", "deleteOne", "deleteMany":
		atomic.AddUint64(&c.DeleteOps, uint64(count))
		c.DeleteHist.RecordBatch(ms, count)
	case "aggregate":
		atomic.AddUint64(&c.AggOps, uint64(count))
		c.AggHist.RecordBatch(ms, count)
	case "transaction":
		atomic.AddUint64(&c.TransOps, uint64(count))
		c.TransHist.RecordBatch(ms, count)
	}
}

const monitorLayout = " %-7s | %9s | %7s | %7s | %7s | %7s | %7s | %6s | %5s\n"

func (c *Collector) Monitor(done <-chan struct{}, refreshRateSec int, concurrency int) {
	ticker := time.NewTicker(time.Duration(refreshRateSec) * time.Second)
	defer ticker.Stop()

	fmt.Println()
	fmt.Println(logger.GreenString("> Starting Workload..."))
	fmt.Println()

	header := fmt.Sprintf(monitorLayout, "TIME", "TOTAL OPS", "SELECT", "INSERT", "UPSERT", "UPDATE", "DELETE", "AGG", "TRANS")
	fmt.Print(logger.BoldString(header))

	fmt.Println(logger.CyanString(
		" -----------------------------------------------------------------------------------------",
	))

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c.printInterval()
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

func (c *Collector) PrintFinalSummary(duration time.Duration) {
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
