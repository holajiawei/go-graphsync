package graphsync_test

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ipfs/go-blockservice"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-graphsync/benchmarks/testinstance"
	tn "github.com/ipfs/go-graphsync/benchmarks/testnet"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	chunker "github.com/ipfs/go-ipfs-chunker"
	delay "github.com/ipfs/go-ipfs-delay"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	files "github.com/ipfs/go-ipfs-files"
	ipldformat "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	"github.com/ipfs/go-unixfs/importer/balanced"
	ihelper "github.com/ipfs/go-unixfs/importer/helpers"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	ipldselector "github.com/ipld/go-ipld-prime/traversal/selector"
	"github.com/ipld/go-ipld-prime/traversal/selector/builder"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/stretchr/testify/require"
)

const stdBlockSize = 8000

type runStats struct {
	Time time.Duration
	Name string
}

var benchmarkLog []runStats

func BenchmarkRoundtripSuccess(b *testing.B) {
	ctx := context.Background()
	tdm, err := newTempDirMaker(b)
	require.NoError(b, err)
	b.Run("test-20-10000", func(b *testing.B) {
		subtestDistributeAndFetch(ctx, b, 20, delay.Fixed(0), time.Duration(0), allFilesUniformSize(10000, defaultUnixfsChunkSize, defaultUnixfsLinksPerLevel), tdm)
	})
	b.Run("test-p2p-stress-10-128MB", func(b *testing.B) {
		p2pStrestTest(ctx, b, 20, allFilesUniformSize(128*(1<<20), 1<<20, 1024), tdm)
	})
}

func p2pStrestTest(ctx context.Context, b *testing.B, numfiles int, df distFunc, tdm *tempDirMaker) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mn := mocknet.New(ctx)
	mn.SetLinkDefaults(mocknet.LinkOptions{Latency: 100 * time.Millisecond, Bandwidth: 3000000})
	net := tn.StreamNet(ctx, mn)
	ig := testinstance.NewTestInstanceGenerator(ctx, net, nil, tdm)
	instances, err := ig.Instances(1 + b.N)
	require.NoError(b, err)
	var allCids []cid.Cid
	for i := 0; i < numfiles; i++ {
		thisCids := df(ctx, b, instances[:1])
		allCids = append(allCids, thisCids...)
	}
	ssb := builder.NewSelectorSpecBuilder(basicnode.Style.Any)

	allSelector := ssb.ExploreRecursive(ipldselector.RecursionLimitNone(),
		ssb.ExploreAll(ssb.ExploreRecursiveEdge())).Node()

	runtime.GC()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fetcher := instances[i+1]
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		require.NoError(b, err)
		start := time.Now()
		disconnectOn := rand.Intn(numfiles)
		for j := 0; j < numfiles; j++ {
			resultChan, errChan := fetcher.Exchange.Request(ctx, instances[0].Peer, cidlink.Link{Cid: allCids[j]}, allSelector)

			wg.Add(1)
			go func(j int) {
				defer wg.Done()
				results := 0
				for {
					select {
					case <-ctx.Done():
						return
					case <-resultChan:
						results++
						if results == 100 && j == disconnectOn {
							mn.DisconnectPeers(instances[0].Peer, instances[i+1].Peer)
							mn.UnlinkPeers(instances[0].Peer, instances[i+1].Peer)
							time.Sleep(100 * time.Millisecond)
							mn.LinkPeers(instances[0].Peer, instances[i+1].Peer)
						}
					case err, ok := <-errChan:
						if !ok {
							return
						}
						b.Fatalf("received error on request: %s", err.Error())
					}
				}
			}(j)
		}
		wg.Wait()
		result := runStats{
			Time: time.Since(start),
			Name: b.Name(),
		}
		benchmarkLog = append(benchmarkLog, result)
		cancel()
		fetcher.Close()
	}
	testinstance.Close(instances)
	ig.Close()
}
func subtestDistributeAndFetch(ctx context.Context, b *testing.B, numnodes int, d delay.D, bstoreLatency time.Duration, df distFunc, tdm *tempDirMaker) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	net := tn.VirtualNetwork(d)
	ig := testinstance.NewTestInstanceGenerator(ctx, net, nil, tdm)
	instances, err := ig.Instances(numnodes + b.N)
	require.NoError(b, err)
	destCids := df(ctx, b, instances[:numnodes])
	// Set the blockstore latency on seed nodes
	if bstoreLatency > 0 {
		for _, i := range instances {
			i.SetBlockstoreLatency(bstoreLatency)
		}
	}
	ssb := builder.NewSelectorSpecBuilder(basicnode.Style.Any)

	allSelector := ssb.ExploreRecursive(ipldselector.RecursionLimitNone(),
		ssb.ExploreAll(ssb.ExploreRecursiveEdge())).Node()

	runtime.GC()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fetcher := instances[i+numnodes]
		var wg sync.WaitGroup
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		require.NoError(b, err)
		start := time.Now()
		for j := 0; j < numnodes; j++ {
			instance := instances[j]
			_, errChan := fetcher.Exchange.Request(ctx, instance.Peer, cidlink.Link{Cid: destCids[j]}, allSelector)

			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case err, ok := <-errChan:
						if !ok {
							return
						}
						b.Fatalf("received error on request: %s", err.Error())
					}
				}
			}()
		}
		wg.Wait()
		result := runStats{
			Time: time.Since(start),
			Name: b.Name(),
		}
		benchmarkLog = append(benchmarkLog, result)
		cancel()
		fetcher.Close()
	}
	testinstance.Close(instances)
	ig.Close()

}

type distFunc func(ctx context.Context, b *testing.B, provs []testinstance.Instance) []cid.Cid

const defaultUnixfsChunkSize uint64 = 1 << 10
const defaultUnixfsLinksPerLevel = 1024

func loadRandomUnixFxFile(ctx context.Context, b *testing.B, bs blockstore.Blockstore, size uint64, unixfsChunkSize uint64, unixfsLinksPerLevel int) cid.Cid {

	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(b, err)
	buf := bytes.NewReader(data)
	file := files.NewReaderFile(buf)

	dagService := merkledag.NewDAGService(blockservice.New(bs, offline.Exchange(bs)))

	// import to UnixFS
	bufferedDS := ipldformat.NewBufferedDAG(ctx, dagService)

	params := ihelper.DagBuilderParams{
		Maxlinks:   unixfsLinksPerLevel,
		RawLeaves:  true,
		CidBuilder: nil,
		Dagserv:    bufferedDS,
	}

	db, err := params.New(chunker.NewSizeSplitter(file, int64(unixfsChunkSize)))
	require.NoError(b, err, "unable to setup dag builder")

	nd, err := balanced.Layout(db)
	require.NoError(b, err, "unable to create unix fs node")

	err = bufferedDS.Commit()
	require.NoError(b, err, "unable to commit unix fs node")

	return nd.Cid()
}

func allFilesUniformSize(size uint64, unixfsChunkSize uint64, unixfsLinksPerLevel int) distFunc {
	return func(ctx context.Context, b *testing.B, provs []testinstance.Instance) []cid.Cid {
		cids := make([]cid.Cid, 0, len(provs))
		for _, prov := range provs {
			c := loadRandomUnixFxFile(ctx, b, prov.BlockStore, size, unixfsChunkSize, unixfsLinksPerLevel)
			cids = append(cids, c)
		}
		return cids
	}
}

type tempDirMaker struct {
	tdm        string
	tempDirSeq int32
	b          *testing.B
}

var tempDirReplacer struct {
	sync.Once
	r *strings.Replacer
}

// Cribbed from https://github.com/golang/go/blob/master/src/testing/testing.go#L890
// and modified as needed due to https://github.com/golang/go/issues/41062
func newTempDirMaker(b *testing.B) (*tempDirMaker, error) {
	c := &tempDirMaker{}
	// ioutil.TempDir doesn't like path separators in its pattern,
	// so mangle the name to accommodate subtests.
	tempDirReplacer.Do(func() {
		tempDirReplacer.r = strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	})
	pattern := tempDirReplacer.r.Replace(b.Name())

	var err error
	c.tdm, err = ioutil.TempDir("", pattern)
	if err != nil {
		return nil, err
	}
	b.Cleanup(func() {
		if err := os.RemoveAll(c.tdm); err != nil {
			b.Errorf("TempDir RemoveAll cleanup: %v", err)
		}
	})
	return c, nil
}

func (tdm *tempDirMaker) TempDir() string {
	seq := atomic.AddInt32(&tdm.tempDirSeq, 1)
	dir := fmt.Sprintf("%s%c%03d", tdm.tdm, os.PathSeparator, seq)
	if err := os.Mkdir(dir, 0777); err != nil {
		tdm.b.Fatalf("TempDir: %v", err)
	}
	return dir
}
