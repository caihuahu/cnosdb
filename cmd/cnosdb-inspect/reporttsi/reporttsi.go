package reporttsi

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync/atomic"
	"text/tabwriter"

	"github.com/cnosdb/cnosdb/pkg/logger"
	"github.com/cnosdb/cnosdb/vend/db/tsdb"
	"github.com/cnosdb/cnosdb/vend/db/tsdb/index/tsi1"

	"github.com/spf13/cobra"
)

const (
	// Number of series IDs to stored in slice before we convert to a roaring
	// bitmap. Roaring bitmaps have a non-trivial initial cost to construct.
	useBitmapN = 25
)

// Option represents the program execution for "cnosdb reporttsi".
type Option struct {
	dbPath        string
	shardPaths    map[uint64]string
	shardIdxs     map[uint64]*tsi1.Index
	cardinalities map[uint64]map[string]*cardinality

	seriesFilePath string // optional. Defaults to dbPath/_series
	sfile          *tsdb.SeriesFile

	topN          int
	byMeasurement bool
	byTagKey      bool

	// How many goroutines to dedicate to calculating cardinality.
	concurrency int
}

var opt = Option{
	shardPaths:    map[uint64]string{},
	shardIdxs:     map[uint64]*tsi1.Index{},
	cardinalities: map[uint64]map[string]*cardinality{},
	topN:          0,
	byMeasurement: true,
	byTagKey:      false,
	concurrency:   runtime.GOMAXPROCS(0),
}

func GetCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "report-tsi",
		Short: "Reports on cardinality for shards and measurements.",
		Run: func(cmd *cobra.Command, args []string) {
			if opt.byTagKey {
				cmd.PrintErrln("Segmenting cardinality by tag key is not yet implemented")
				return
			}

			if opt.dbPath == "" {
				cmd.PrintErrln("path to database must be provided")
				return
			}

			if opt.seriesFilePath == "" {
				opt.seriesFilePath = path.Join(opt.dbPath, tsdb.SeriesFileDirectory)
			}

			// Walk database directory to get shards.
			if err := filepath.Walk(opt.dbPath, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}

				if !info.IsDir() {
					return nil
				}

				if info.Name() == tsdb.SeriesFileDirectory || info.Name() == "index" {
					return filepath.SkipDir
				}

				id, err := strconv.Atoi(info.Name())
				if err != nil {
					return nil
				}
				opt.shardPaths[uint64(id)] = path
				return nil
			}); err != nil {
				return
			}

			if len(opt.shardPaths) == 0 {
				cmd.PrintErrf("No shards under %s\n", opt.dbPath)
				return
			}

			if err := run(cmd); err != nil {
				cmd.PrintErrln(err)
			}
		},
	}

	c.PersistentFlags().StringVar(&opt.dbPath, "db-path", "", "Path to database. Required.")
	c.PersistentFlags().StringVar(&opt.seriesFilePath, "series-file", "", "Optional path to series file. Defaults /path/to/db-path/_series")
	c.PersistentFlags().BoolVar(&opt.byMeasurement, "measurements", true, "Segment cardinality by measurements")
	// fs.BoolVar(&cmd.byTagKey, "tag-key", false, "Segment cardinality by tag keys (overrides `measurements`")
	c.PersistentFlags().IntVar(&opt.topN, "top", 0, "Limit results to top n")
	c.PersistentFlags().IntVar(&opt.concurrency, "c", runtime.GOMAXPROCS(0), "Set worker concurrency. Defaults to GOMAXPROCS setting.")

	return c
}

func run(cmd *cobra.Command) error {
	opt.sfile = tsdb.NewSeriesFile(opt.seriesFilePath)
	opt.sfile.Logger = logger.NewLoggerWithWriter(os.Stderr)
	if err := opt.sfile.Open(); err != nil {
		return err
	}
	defer opt.sfile.Close()

	// Open all the indexes.
	for id, pth := range opt.shardPaths {
		pth = path.Join(pth, "index")
		// Verify directory is an index before opening it.
		if ok, err := tsi1.IsIndexDir(pth); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("not a TSI index directory: %q", pth)
		}

		opt.shardIdxs[id] = tsi1.NewIndex(opt.sfile,
			"",
			tsi1.WithPath(pth),
			tsi1.DisableCompactions(),
		)
		if err := opt.shardIdxs[id].Open(); err != nil {
			return err
		}
		defer opt.shardIdxs[id].Close()

		// Initialise cardinality set to store cardinalities for this shard.
		opt.cardinalities[id] = map[string]*cardinality{}
	}

	// Calculate cardinalities of shards.
	fn := opt.cardinalityByMeasurement
	// if cmd.byTagKey {
	// TODO
	// }

	// Blocks until all work done.
	opt.calculateCardinalities(fn)

	// Print summary.
	if err := opt.printSummaryByMeasurement(cmd); err != nil {
		return err
	}

	allIDs := make([]uint64, 0, len(opt.shardIdxs))
	for id := range opt.shardIdxs {
		allIDs = append(allIDs, id)
	}
	sort.Slice(allIDs, func(i int, j int) bool { return allIDs[i] < allIDs[j] })

	for _, id := range allIDs {
		if err := opt.printShardByMeasurement(id, cmd); err != nil {
			return err
		}
	}
	return nil
}

// calculateCardinalities calculates the cardinalities of the set of shard being
// worked on concurrently. The provided function determines how cardinality is
// calculated and broken down.
func (opt *Option) calculateCardinalities(fn func(id uint64) error) error {
	// Get list of shards to work on.
	shardIDs := make([]uint64, 0, len(opt.shardIdxs))
	for id := range opt.shardIdxs {
		shardIDs = append(shardIDs, id)
	}

	errC := make(chan error, len(shardIDs))
	var maxi uint32 // index of maximumm shard being worked on.
	for k := 0; k < opt.concurrency; k++ {
		go func() {
			for {
				i := int(atomic.AddUint32(&maxi, 1) - 1) // Get next partition to work on.
				if i >= len(shardIDs) {
					return // No more work.
				}
				errC <- fn(shardIDs[i])
			}
		}()
	}

	// Check for error
	for i := 0; i < cap(errC); i++ {
		if err := <-errC; err != nil {
			return err
		}
	}
	return nil
}

type cardinality struct {
	name  []byte
	short []uint32
	set   *tsdb.SeriesIDSet
}

func (c *cardinality) add(x uint64) {
	if c.set != nil {
		c.set.AddNoLock(x)
		return
	}

	c.short = append(c.short, uint32(x)) // Series IDs never get beyond 2^32

	// Cheaper to store in bitmap.
	if len(c.short) > useBitmapN {
		c.set = tsdb.NewSeriesIDSet()
		for i := 0; i < len(c.short); i++ {
			c.set.AddNoLock(uint64(c.short[i]))
		}
		c.short = nil
		return
	}
}

func (c *cardinality) cardinality() int64 {
	if c == nil || (c.short == nil && c.set == nil) {
		return 0
	}

	if c.short != nil {
		return int64(len(c.short))
	}
	return int64(c.set.Cardinality())
}

type cardinalities []*cardinality

func (a cardinalities) Len() int           { return len(a) }
func (a cardinalities) Less(i, j int) bool { return a[i].cardinality() < a[j].cardinality() }
func (a cardinalities) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

func (opt *Option) cardinalityByMeasurement(shardID uint64) error {
	idx := opt.shardIdxs[shardID]
	itr, err := idx.MeasurementIterator()
	if err != nil {
		return err
	} else if itr == nil {
		return nil
	}
	defer itr.Close()

OUTER:
	for {
		name, err := itr.Next()
		if err != nil {
			return err
		} else if name == nil {
			break OUTER
		}

		// Get series ID set to track cardinality under measurement.
		c, ok := opt.cardinalities[shardID][string(name)]
		if !ok {
			c = &cardinality{name: name}
			opt.cardinalities[shardID][string(name)] = c
		}

		sitr, err := idx.MeasurementSeriesIDIterator(name)
		if err != nil {
			return err
		} else if sitr == nil {
			continue
		}

		var e tsdb.SeriesIDElem
		for e, err = sitr.Next(); err == nil && e.SeriesID != 0; e, err = sitr.Next() {
			if e.SeriesID > math.MaxUint32 {
				panic(fmt.Sprintf("series ID is too large: %d (max %d). Corrupted series file?", e.SeriesID, uint32(math.MaxUint32)))
			}
			c.add(e.SeriesID)
		}
		sitr.Close()

		if err != nil {
			return err
		}
	}
	return nil
}

type result struct {
	name  []byte
	count int64

	// For low cardinality measurements just track series using map
	lowCardinality map[uint32]struct{}

	// For higher cardinality measurements track using bitmap.
	set *tsdb.SeriesIDSet
}

func (r *result) addShort(ids []uint32) {
	// There is already a bitset of this result.
	if r.set != nil {
		for _, id := range ids {
			r.set.AddNoLock(uint64(id))
		}
		return
	}

	// Still tracking low cardinality sets
	if r.lowCardinality == nil {
		r.lowCardinality = map[uint32]struct{}{}
	}

	for _, id := range ids {
		r.lowCardinality[id] = struct{}{}
	}

	// Cardinality is large enough that we will benefit from using a bitmap
	if len(r.lowCardinality) > useBitmapN {
		r.set = tsdb.NewSeriesIDSet()
		for id := range r.lowCardinality {
			r.set.AddNoLock(uint64(id))
		}
		r.lowCardinality = nil
	}
}

func (r *result) merge(other *tsdb.SeriesIDSet) {
	if r.set == nil {
		r.set = tsdb.NewSeriesIDSet()
		for id := range r.lowCardinality {
			r.set.AddNoLock(uint64(id))
		}
		r.lowCardinality = nil
	}
	r.set.Merge(other)
}

type results []*result

func (a results) Len() int           { return len(a) }
func (a results) Less(i, j int) bool { return a[i].count < a[j].count }
func (a results) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

func (opt *Option) printSummaryByMeasurement(cmd *cobra.Command) error {
	// Get global set of measurement names across shards.
	idxs := &tsdb.IndexSet{SeriesFile: opt.sfile}
	for _, idx := range opt.shardIdxs {
		idxs.Indexes = append(idxs.Indexes, idx)
	}

	mitr, err := idxs.MeasurementIterator()
	if err != nil {
		return err
	} else if mitr == nil {
		return errors.New("got nil measurement iterator for index set")
	}
	defer mitr.Close()

	var name []byte
	var totalCardinality int64
	measurements := results{}
	for name, err = mitr.Next(); err == nil && name != nil; name, err = mitr.Next() {
		res := &result{name: name}
		for _, shardCards := range opt.cardinalities {
			other, ok := shardCards[string(name)]
			if !ok {
				continue // this shard doesn't have anything for this measurement.
			}

			if other.short != nil && other.set != nil {
				panic("cardinality stored incorrectly")
			}

			if other.short != nil { // low cardinality case
				res.addShort(other.short)
			} else if other.set != nil { // High cardinality case
				res.merge(other.set)
			}

			// Shard does not have any series for this measurement.
		}

		// Determine final cardinality and allow intermediate structures to be
		// GCd.
		if res.lowCardinality != nil {
			res.count = int64(len(res.lowCardinality))
		} else {
			res.count = int64(res.set.Cardinality())
		}
		totalCardinality += res.count
		res.set = nil
		res.lowCardinality = nil
		measurements = append(measurements, res)
	}

	if err != nil {
		return err
	}

	// sort measurements by cardinality.
	sort.Sort(sort.Reverse(measurements))

	if opt.topN > 0 {
		// There may not be "topN" measurement cardinality to sub-slice.
		n := int(math.Min(float64(opt.topN), float64(len(measurements))))
		measurements = measurements[:n]
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 4, 4, 1, '\t', 0)
	fmt.Fprintf(tw, "Summary\nDatabase Path: %s\nCardinality (exact): %d\n\n", opt.dbPath, totalCardinality)
	fmt.Fprint(tw, "Measurement\tCardinality (exact)\n\n")
	for _, res := range measurements {
		fmt.Fprintf(tw, "%q\t\t%d\n", res.name, res.count)
	}

	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), "\n\n")
	return nil
}

func (opt *Option) printShardByMeasurement(id uint64, cmd *cobra.Command) error {
	allMap, ok := opt.cardinalities[id]
	if !ok {
		return nil
	}

	var totalCardinality int64
	all := make(cardinalities, 0, len(allMap))
	for _, card := range allMap {
		n := card.cardinality()
		if n == 0 {
			continue
		}

		totalCardinality += n
		all = append(all, card)
	}

	sort.Sort(sort.Reverse(all))

	// Trim to top-n
	if opt.topN > 0 {
		// There may not be "topN" measurement cardinality to sub-slice.
		n := int(math.Min(float64(opt.topN), float64(len(all))))
		all = all[:n]
	}

	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 4, 4, 1, '\t', 0)
	fmt.Fprintf(tw, "===============\nShard ID: %d\nPath: %s\nCardinality (exact): %d\n\n", id, opt.shardPaths[id], totalCardinality)
	fmt.Fprint(tw, "Measurement\tCardinality (exact)\n\n")
	for _, card := range all {
		fmt.Fprintf(tw, "%q\t\t%d\n", card.name, card.cardinality())
	}
	fmt.Fprint(tw, "===============\n\n")
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), "\n\n")
	return nil
}
