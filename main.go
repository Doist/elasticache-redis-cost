// elasticache-redis-cost suggests AWS ElastiCache instance types that can fit
// existing Redis instances
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/pricing"
	"github.com/go-redis/redis/v8"
	"github.com/jmespath/go-jmespath"
	"golang.org/x/sync/errgroup"
)

func main() {
	log.SetFlags(0)
	args := runArgs{region: "us-east-1", maxLoadPct: 100}
	flag.StringVar(&args.region, "region", args.region,
		"use prices for this AWS `region`")
	flag.StringVar(&args.input, "redises", "",
		"`path` to file with Redis addresses, one per line (/dev/stdin to read from stdin)")
	flag.StringVar(&args.html, "html", args.html,
		"`path` to HTML file to save report; if empty, text-only report is printed to stdout")
	flag.BoolVar(&args.withOldGen, "any-generation", args.withOldGen,
		"take into account old generation instance types")
	flag.BoolVar(&args.anyFamily, "any-family", args.anyFamily,
		"take into account all instance families, not only memory-optimized")
	flag.IntVar(&args.maxLoadPct, "max-load", args.maxLoadPct, "target this `percent` memory utilization, [1,100] range")
	flag.Parse()
	if err := run(args); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
}

type runArgs struct {
	region     string
	input      string
	html       string
	withOldGen bool
	anyFamily  bool
	maxLoadPct int
}

func (args runArgs) validate() error {
	if args.region == "" {
		return errors.New("region cannot be empty")
	}
	if args.input == "" {
		return errors.New("input file must be set")
	}
	if args.maxLoadPct < 1 || args.maxLoadPct > 100 {
		return errors.New("max-load must be in [1,100] percent range")
	}
	return nil
}

func run(args runArgs) error {
	if err := args.validate(); err != nil {
		return err
	}

	region, ok := endpoints.AwsPartition().Regions()[args.region]
	if !ok {
		return fmt.Errorf("unsupported region %q", args.region)
	}

	f, err := os.Open(args.input)
	if err != nil {
		return err
	}
	defer f.Close()
	redises, err := readAddresses(f)
	if err != nil {
		return err
	}
	f.Close()

	if len(redises) == 0 {
		return errors.New("no Redis addresses to work on")
	}

	ctx := context.Background()
	sess, err := session.NewSession()
	if err != nil {
		return err
	}

	maxWorkers := len(redises)
	const workerCap = 10
	if maxWorkers > workerCap {
		maxWorkers = workerCap
	}
	redisesInfo := make([]RedisStats, len(redises)) // preallocate for concurrent fill
	type addrAndIndex struct {
		addr  string
		index int
	}
	jobs := make(chan addrAndIndex)
	var offerings Offerings

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		defer close(jobs)
		for i, addr := range redises {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case jobs <- addrAndIndex{addr: addr, index: i}:
			}
		}
		return nil
	})
	for i := 0; i < maxWorkers; i++ {
		group.Go(func() error {
			for job := range jobs {
				used, peak, err := redisMemory(ctx, job.addr)
				if err != nil {
					return fmt.Errorf("%s: %w", job.addr, err)
				}
				redisesInfo[job.index] = RedisStats{Addr: job.addr, UsedBytes: used, PeakBytes: peak}
			}
			return nil
		})
	}
	pricingFilters := []*pricing.Filter{
		{
			Field: aws.String("cacheEngine"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("Redis"),
		},
		{
			Field: aws.String("location"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String(region.Description()),
		},
	}
	if !args.anyFamily {
		pricingFilters = append(pricingFilters, &pricing.Filter{
			Field: aws.String("instanceFamily"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("Memory optimized"),
		})
	}
	if !args.withOldGen {
		pricingFilters = append(pricingFilters, &pricing.Filter{
			Field: aws.String("currentGeneration"),
			Type:  aws.String("TERM_MATCH"),
			Value: aws.String("yes"),
		})
	}
	group.Go(func() error {
		svc := pricing.New(sess)
		res, err := svc.GetProductsWithContext(ctx, &pricing.GetProductsInput{
			ServiceCode: aws.String("AmazonElastiCache"),
			Filters:     pricingFilters,
		})
		if err != nil {
			return err
		}
		for _, priceList := range res.PriceList {
			memory, err := extractMemory(priceList["product"])
			if err != nil {
				return err
			}
			instanceType, err := extractInstanceType(priceList["product"])
			if err != nil {
				return err
			}
			price, err := extractPrice(priceList["terms"])
			if err != nil {
				return err
			}
			if mem, ok := maxmemoryValues[instanceType]; ok {
				memory = mem
			} else {
				memory = memory / 4 * 3 // maxmemory is 75% of total mem
				log.Printf("maxmemory value for instance %q is unknown,"+
					" using default as 75%% of instance size", instanceType)
			}
			offerings = append(offerings, Offering{
				Memory:       memory,
				PricePerHour: price,
				InstanceType: instanceType,
			})
		}
		offerings.sortByMemory()
		return nil
	})

	if err := group.Wait(); err != nil {
		return err
	}

	type row struct {
		Redis     RedisStats
		UsedRatio float64
		PeakRatio float64
		UsedBased Offering
		PeakBased Offering
	}

	rows := make([]row, 0, len(redisesInfo))
	for _, ri := range redisesInfo {
		plan1, err := offerings.match(ri.UsedBytes, args.maxLoadPct)
		if err != nil {
			return fmt.Errorf("no matching plad for %q with %d GiB of used memory: %w", ri.Addr, ri.UsedBytes<<30, err)
		}
		plan2, err := offerings.match(ri.PeakBytes, args.maxLoadPct)
		if err != nil {
			return fmt.Errorf("no matching plad for %q with %d GiB of peak memory: %w", ri.Addr, ri.PeakBytes<<30, err)
		}
		rows = append(rows, row{
			Redis:     ri,
			UsedRatio: float64(ri.UsedBytes) / float64(plan1.Memory) * 100,
			PeakRatio: float64(ri.PeakBytes) / float64(plan2.Memory) * 100,
			UsedBased: plan1,
			PeakBased: plan2,
		})
	}
	if args.html == "" {
		tw := tabwriter.NewWriter(os.Stdout, 1, 4, 1, ' ', 0)
		defer tw.Flush()
		fmt.Fprintf(tw, "HOST\tUSED(LOAD)\tTYPE\t$/HR\t$/MONTH\tPEAK(LOAD)\tTYPE\t$/HR\t$/MONTH\t\n")
		for _, row := range rows {
			fmt.Fprintf(tw, "%s\t%.1f (%.1f%%)\t%s\t%.3f\t%.3f\t%.1f (%.1f%%)\t%s\t%.3f\t%.3f\t\n", row.Redis.Addr,
				row.Redis.UsedGiB(), row.UsedRatio,
				row.UsedBased.InstanceType, row.UsedBased.PricePerHour, row.UsedBased.PricePerMonth(),
				row.Redis.PeakGiB(), row.PeakRatio,
				row.PeakBased.InstanceType, row.PeakBased.PricePerHour, row.PeakBased.PricePerMonth(),
			)
		}
		return nil
	}
	page := struct {
		Rows           []row
		UsedBasedTotal float64
		PeakBasedTotal float64
		Time           time.Time
		Region         string
		MaxLoad        int
	}{Rows: rows, Time: time.Now().UTC(), Region: region.Description(), MaxLoad: args.maxLoadPct}
	for _, row := range rows {
		page.UsedBasedTotal += row.UsedBased.PricePerMonth()
		page.PeakBasedTotal += row.PeakBased.PricePerMonth()
	}
	buf := new(bytes.Buffer)
	if err := pageTemplate.Execute(buf, page); err != nil {
		return err
	}
	return ioutil.WriteFile(args.html, buf.Bytes(), 0666)
}

type Offerings []Offering

func (ofs Offerings) sortByMemory() {
	sort.Slice(ofs, func(i, j int) bool { return ofs[i].Memory < ofs[j].Memory })
}

func (ofs Offerings) match(size uint64, maxLoadPct int) (Offering, error) {
	i := sort.Search(len(ofs), func(i int) bool { return ofs[i].Memory/100*uint64(maxLoadPct) >= size })
	if i < len(ofs) {
		return ofs[i], nil
	}
	return Offering{}, errors.New("no matching offering found")
}

type Offering struct {
	Memory       uint64
	PricePerHour float64
	InstanceType string
}

func (o Offering) PricePerMonth() float64 {
	return o.PricePerHour * 24 * 31
}

func (o Offering) MemoryGiB() float64 {
	return float64(o.Memory>>20) / 1024
}

type RedisStats struct {
	Addr      string
	UsedBytes uint64
	PeakBytes uint64
}

func (s RedisStats) UsedGiB() float64 { return float64(s.UsedBytes>>20) / 1024 }
func (s RedisStats) PeakGiB() float64 { return float64(s.PeakBytes>>20) / 1024 }

var queryPrice = jmespath.MustCompile("OnDemand.*[].priceDimensions.*[].pricePerUnit.USD | [0]")
var queryIstanceType = jmespath.MustCompile("attributes.instanceType")
var queryMemory = jmespath.MustCompile("attributes.memory")

func extractInstanceType(data interface{}) (string, error) {
	raw, err := queryIstanceType.Search(data)
	if err != nil {
		return "", err
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("cannot convert %T / %+v to string", raw, raw)
	}
	return s, nil
}

func extractMemory(data interface{}) (uint64, error) {
	raw, err := queryMemory.Search(data)
	if err != nil {
		return 0, err
	}
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("cannot convert %T / %+v to string", raw, raw)
	}
	const suffix = " GiB"
	if !strings.HasSuffix(s, suffix) {
		return 0, fmt.Errorf("unsupported memory spec format, want \"XXX GiB\", got %q", s)
	}
	gibs, err := strconv.ParseFloat(strings.TrimSuffix(s, suffix), 32)
	if err != nil {
		return 0, err
	}
	if gibs <= 0 {
		return 0, fmt.Errorf("unexpected memory value %v (%q)", gibs, s)
	}
	return uint64(gibs * 1024 * 1024 * 1024), nil
}

func extractPrice(data interface{}) (float64, error) {
	raw, err := queryPrice.Search(data)
	if err != nil {
		return 0, err
	}
	s, ok := raw.(string)
	if !ok {
		return 0, fmt.Errorf("cannot convert %T / %+v to string", raw, raw)
	}
	return strconv.ParseFloat(s, 64)
}

func redisMemory(ctx context.Context, addr string) (uint64, uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	data, err := client.Info(ctx, "memory").Bytes()
	if err != nil {
		return 0, 0, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	var used, peak uint64
	for scanner.Scan() {
		const usedPrefix = "used_memory:"
		const peakPrefix = "used_memory_peak:"
		var err error
		switch b := scanner.Bytes(); {
		case bytes.HasPrefix(b, []byte(usedPrefix)):
			if used, err = strconv.ParseUint(string(b[len(usedPrefix):]), 10, 64); err != nil {
				return 0, 0, err
			}
		case bytes.HasPrefix(b, []byte(peakPrefix)):
			if peak, err = strconv.ParseUint(string(b[len(peakPrefix):]), 10, 64); err != nil {
				return 0, 0, err
			}
		}
		if used > 0 && peak > 0 {
			break
		}
	}
	return used, peak, scanner.Err()
}

func readAddresses(rd io.Reader) ([]string, error) {
	var out []string
	scanner := bufio.NewScanner(rd)
	for scanner.Scan() {
		if b := scanner.Bytes(); bytes.HasPrefix(b, []byte("#")) || len(b) == 0 {
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		host, port, err := net.SplitHostPort(line)
		if err != nil {
			return nil, err
		}
		if host == "" || port == "" {
			return nil, fmt.Errorf("%q does not look like a valid address in HOST:PORT format", line)
		}
		out = append(out, line)
	}
	return out, scanner.Err()
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html><head><meta charset="utf-8">
<title>Redis instances matched to ElastiCache Redis instances</title>
<style>
	html {line-height: 1.3; font-family: ui-serif, serif;}
	table, code {font-family: ui-monospace, monospace;}
	caption {padding:1em; caption-side: top; font-weight: bold; font-family: ui-sans-serif, sans-serif;}
	th, td {padding: 0.1rem .5rem;}
	td {white-space: nowrap;}
	th {vertical-align: middle; text-align: center; background-color: #eee;}
	tr:nth-child(even) td {background-color: #f8f8f8;}
	tr:hover td {background-color: #eee;}
	.right {text-align: right;}
	tfoot td {font-weight: bold;}
</style>
</head>
<body>
<table>
<caption>Estimate on ElastiCache instances required to cover Redis instances<br>
based on memory readings from {{.Time.Format "2006-01-02 15:04"}} UTC,
using {{.MaxLoad}}% max memory load target,<br>
prices are for on-demand nodes in {{.Region}} region
</caption>
<thead>
<tr>
	<th rowspan=2>Redis instance</th>
	<th rowspan=2>Used, GiB</th>
	<th rowspan=2>Peak, GiB</th>
	<th colspan=5>Based on used memory</th>
	<th colspan=5>Based on peak memory</th>
</tr>
<tr>
	<!-- 3 columns skipped -->
	<!-- based on used memory -->
	<th>Node type</th>
	<th>Node size, <a href="#footnote">GiB</a><sup>*</sup></th>
	<th>Load, %</th>
	<th>USD<wbr>/hour</th>
	<th>USD<wbr>/month</th>
	<!-- based on peak memory -->
	<th>Node type</th>
	<th>Node size, <a href="#footnote">GiB</a><sup>*</sup></th>
	<th>Load, %</th>
	<th>USD<wbr>/hour</th>
	<th>USD<wbr>/month</th>
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr>
	<td>{{.Redis.Addr}}</td><!-- instance address -->
	<td class="right">{{printf "%.1f" .Redis.UsedGiB}}</td><!-- used memory, GiB -->
	<td class="right">{{printf "%.1f" .Redis.PeakGiB}}</td><!-- peak memory, GiB -->
	<!-- based on used memory -->
	<td>{{.UsedBased.InstanceType}}</td>
	<td class="right">{{printf "%.1f" .UsedBased.MemoryGiB}}</td>
	<td class="right">{{printf "%.1f" .UsedRatio}}</td>
	<td class="right">{{printf "%.3f" .UsedBased.PricePerHour}}</td>
	<td class="right">{{printf "%.3f" .UsedBased.PricePerMonth}}</td>
	<!-- based on peak memory -->
	<td>{{.PeakBased.InstanceType}}</td>
	<td class="right">{{printf "%.1f" .PeakBased.MemoryGiB}}</td>
	<td class="right">{{printf "%.1f" .PeakRatio}}</td>
	<td class="right">{{printf "%.3f" .PeakBased.PricePerHour}}</td>
	<td class="right">{{printf "%.3f" .PeakBased.PricePerMonth}}</td>
</tr>
{{end}}
</tbody>
<tfoot>
<tr>
	<th scope="row" colspan=3>Totals</th>
	<th scope="row" colspan=4>Based on used memory, USD / month</th>
	<td class="right">{{printf "%.3f" .UsedBasedTotal}}</td>
	<th scope="row" colspan=4>Based on peak memory, USD / month</th>
	<td class="right">{{printf "%.3f" .PeakBasedTotal}}</td>
</tr>
</tfoot>
</table>
<footer><p id="footnote"><sup>*</sup> Node sizes displays
<code>maxmemory</code> available for Redis data, taken from
<a href="https://docs.aws.amazon.com/AmazonElastiCache/latest/red-ug/ParameterGroups.Redis.html#ParameterGroups.Redis.NodeSpecific">https://docs.aws.amazon.com/AmazonElastiCache/latest/red-ug/ParameterGroups.Redis.html#ParameterGroups.Redis.NodeSpecific</a>.
</p></footer>
</body>
`))

//go:generate go run ./parse-maxmemory maxmemory.go
