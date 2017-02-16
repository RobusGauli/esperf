package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spenczar/tdigest"
	"gopkg.in/olivere/elastic.v5"
)

var (
	workers     = flag.Int("c", 50, "Number of concurrent workers.")
	qps         = flag.Int("qps", 10, "Number of requests per second impressed on the server.")
	addr        = flag.String("addr", "http://localhost:9200", "Elastic search HTTP address")
	index       = flag.String("index", "wikipediax", "Index to perform queries")
	resultsPath = flag.String("results_path", "~/results", "")
	expID       = flag.String("exp_id", "1", "")
	duration    = flag.Duration("duration", 30*time.Second, "Time sending load to the server.")
	cint        = flag.Duration("cint", 5*time.Second, "Interval between metrics collection.")
)

type Snapshot struct {
	td tdigest.TDigest
}

func (s *Snapshot) Quantile(q float64) float64 {
	return s.td.Quantile(q)
}

type ResponseTimeStats struct {
	sync.Mutex
	count int64
	td    tdigest.TDigest
	buff  []int64
}

func (s *ResponseTimeStats) Record(v int64) {
	s.Lock()
	defer s.Unlock()
	s.buff = append(s.buff, v)
	s.count++
}

func (s *ResponseTimeStats) Snapshot() (*Snapshot, int64) {
	s.Lock()
	auxBuff := make([]int64, len(s.buff))
	copy(auxBuff, s.buff)
	s.buff = nil
	count := s.count
	s.Unlock()

	td := tdigest.New()
	for _, v := range auxBuff {
		td.Add(float64(v), 1)
	}

	return &Snapshot{td}, count
}

var (
	succs, errs, reqs, shed uint64
	logger                  = log.New(os.Stdout, "", log.LstdFlags|log.Lshortfile)
	respTimeStats           = &ResponseTimeStats{}
)

func main() {
	flag.Parse()
	logger.Printf("Starting sending load: #Workers:%d GlobalQPS:%d Duration:%v\n", *workers, *qps, *duration)

	elastic.SetErrorLog(logger)
	elastic.SetTraceLog(logger)
	elastic.SetInfoLog(logger)
	elastic.SetMaxRetries(0)
	elastic.SetSniff(false)

	endStatsChan := make(chan struct{})
	var statsWaiter sync.WaitGroup
	statsWaiter.Add(1)
	go statsCollector(endStatsChan, &statsWaiter)
	logger.Println("Stats collector started.")

	endLoadChan := make(chan struct{}, *workers)
	pauseLoadChan := make(chan float64, *workers)
	var loadWaiter sync.WaitGroup
	for i := 0; i < *workers; i++ {
		loadWaiter.Add(1)
		go worker(pauseLoadChan, endLoadChan, &loadWaiter)
	}
	logger.Printf("%d load workers have started. Waiting for their hard work...\n", *workers)

	start := time.Now()
	<-time.Tick(*duration)
	for i := 0; i < *workers; i++ {
		endLoadChan <- struct{}{}
	}
	loadWaiter.Wait()
	close(endLoadChan)
	close(pauseLoadChan)

	dur := time.Now().Sub(start).Seconds()
	logger.Printf("Done. QPS:%.2f #REQS:%d #SUCC:%d #ERR:%d #SHED:%d TP:%.2f", float64(reqs)/dur, reqs, succs, errs, shed, float64(succs)/dur)
	atomic.StoreUint64(&reqs, 0)
	atomic.StoreUint64(&succs, 0)
	atomic.StoreUint64(&errs, 0)
	atomic.StoreUint64(&shed, 0)

	logger.Println("Finishing stats collection...")
	endStatsChan <- struct{}{} // send signal to finish stats worker.
	statsWaiter.Wait()         // wait for stats worker to do its stuff.
	close(endStatsChan)
	logger.Printf("Done. Results can be found at %s\n", *resultsPath)
	logger.Println("Load test finished successfully.")
}

func worker(pause chan float64, end chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	numReq := (*qps * int((*duration).Seconds())) / *workers
	if numReq == 0 {
		logger.Fatalf("To few requests per worker, please increase the qps or decrease the number of workers")
	}

	client, err := elastic.NewClient(elastic.SetURL(*addr))
	if err != nil {
		logger.Fatal(err)
	}
	ctx := context.Background()
	fire := time.Tick(time.Duration(float64((*duration).Nanoseconds())/float64(numReq)) * time.Nanosecond)
	for i := 0; i < numReq; i++ {
		select {
		case <-fire:
			sendRequest(ctx, client, pause)
		case pt := <-pause:
			time.Sleep(time.Duration(pt*1000000000) * time.Nanosecond)
			fmt.Printf("Sleeping: %v\n", time.Duration(pt*1000000000)*time.Nanosecond)
			for range pause {
			} // Emptying pause channel before continue.
		case <-end:
			return
		}
	}
}

func sendRequest(ctx context.Context, client *elastic.Client, pauseChan chan float64) {
	atomic.AddUint64(&reqs, 1)
	q := elastic.NewTermQuery("text", "America")
	resp, err := client.Search().Index(*index).Query(q).Do(ctx)
	if err != nil {
		atomic.AddUint64(&errs, 1)
		return
	}
	s := resp.StatusCode
	switch {
	case s == http.StatusOK:
		atomic.AddUint64(&succs, 1)
		respTimeStats.Record(resp.TookInMillis)
	case s == http.StatusTooManyRequests || s == http.StatusServiceUnavailable:
		atomic.AddUint64(&shed, 1)
		ra := resp.Header.Get("Retry-After")
		if ra != "" {
			pt, err := strconv.ParseFloat(ra, 64)
			if err != nil {
				logger.Printf("%q\n", err)
			} else {
				pauseChan <- pt
			}
		}
	default:
		atomic.AddUint64(&errs, 1)
	}
}

func statsCollector(end chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	client, err := elastic.NewClient(elastic.SetURL(*addr))
	if err != nil {
		logger.Fatalf("Error creating ES stats client: %q", err)
	}
	ctx := context.Background()

	mF := newFile("mem.pools")
	defer mF.Close()
	memPools := csv.NewWriter(bufio.NewWriter(mF))
	writeMemHeader(memPools)

	gcF := newFile("gc")
	defer gcF.Close()
	gc := csv.NewWriter(bufio.NewWriter(gcF))
	writeGCHeader(gc)

	tpF := newFile("tp")
	defer tpF.Close()
	tp := csv.NewWriter(bufio.NewWriter(tpF))
	writeTpHeader(tp)

	cpuF := newFile("cpu")
	defer cpuF.Close()
	cpu := csv.NewWriter(bufio.NewWriter(cpuF))
	writeCPUHeader(cpu)

	lF := newFile("latency")
	defer lF.Close()
	latency := csv.NewWriter(bufio.NewWriter(lF))
	writeLatencyHeader(latency)

	nodeStatsService := client.NodesStats().Metric("jvm", "indices", "process")

	collect := func() {
		resp, err := nodeStatsService.Do(ctx)
		if err != nil {
			logger.Printf("%q\n", err)
			return
		}
		var ns *elastic.NodesStatsNode
		for _, ns = range resp.Nodes {
		}
		ts := ns.JVM.Timestamp
		s, count := respTimeStats.Snapshot()
		writeMem(ns, memPools, ts)
		writeGC(ns, gc, ts)
		writeTp(tp, ts, count)
		writeCPU(ns, cpu, ts)
		writeLatency(s, latency, ts)
	}

	fire := time.Tick(*cint)
	for {
		select {
		case <-fire:
			collect()

		case <-end:
			collect()
			return
		}
	}

}

func newFile(fName string) *os.File {
	f, err := os.Create(filepath.Join(*resultsPath, fName+"_"+*expID+".csv"))
	if err != nil {
		logger.Fatal(err)
	}
	return f
}

func writeLatencyHeader(w *csv.Writer) {
	w.Write([]string{"ts", "50perc", "90perc", "99perc", "999perc"})
	w.Flush()
}

func writeLatency(s *Snapshot, w *csv.Writer, ts int64) {
	w.Write([]string{
		strconv.FormatInt(ts, 10),
		fmt.Sprintf("%.2f", float64(s.Quantile(0.5))),
		fmt.Sprintf("%.2f", float64(s.Quantile(0.9))),
		fmt.Sprintf("%.2f", float64(s.Quantile(0.99))),
		fmt.Sprintf("%.2f", float64(s.Quantile(0.999)))})
	w.Flush()
}

func writeTpHeader(w *csv.Writer) {
	w.Write([]string{"ts", "count"})
	w.Flush()
}

func writeTp(w *csv.Writer, ts int64, count int64) {
	w.Write([]string{
		strconv.FormatInt(ts, 10),
		strconv.FormatInt(count, 10)})
	w.Flush()
}

func writeGCHeader(w *csv.Writer) {
	w.Write([]string{"ts", "young.time", "young.count", "old.time", "old.count"})
	w.Flush()
}

func writeGC(stats *elastic.NodesStatsNode, w *csv.Writer, ts int64) {
	collectors := stats.JVM.GC.Collectors
	w.Write([]string{
		strconv.FormatInt(ts, 10),
		strconv.FormatInt(collectors["young"].CollectionTimeInMillis, 10),
		strconv.FormatInt(collectors["young"].CollectionCount, 10),
		strconv.FormatInt(collectors["old"].CollectionTimeInMillis, 10),
		strconv.FormatInt(collectors["old"].CollectionCount, 10)})
	w.Flush()
}

func writeMemHeader(w *csv.Writer) {
	w.Write([]string{"ts", "young.max", "young.used", "survivor.max", "survivor.used", "old.max", "old.used"})
	w.Flush()
}

func writeMem(stats *elastic.NodesStatsNode, w *csv.Writer, ts int64) {
	pools := stats.JVM.Mem.Pools
	w.Write([]string{
		strconv.FormatInt(ts, 10),
		strconv.FormatInt(pools["young"].MaxInBytes, 10),
		strconv.FormatInt(pools["young"].UsedInBytes, 10),
		strconv.FormatInt(pools["survivor"].MaxInBytes, 10),
		strconv.FormatInt(pools["survivor"].UsedInBytes, 10),
		strconv.FormatInt(pools["old"].MaxInBytes, 10),
		strconv.FormatInt(pools["old"].UsedInBytes, 10)})
	w.Flush()
}

func writeCPUHeader(w *csv.Writer) {
	w.Write([]string{"ts", "time", "percent"})
	w.Flush()
}

func writeCPU(stats *elastic.NodesStatsNode, w *csv.Writer, ts int64) {
	w.Write([]string{
		strconv.FormatInt(ts, 10),
		strconv.FormatInt(stats.Process.CPU.TotalInMillis, 10),
		strconv.FormatInt(int64(stats.Process.CPU.Percent), 10)})
	w.Flush()
}
