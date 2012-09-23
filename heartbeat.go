package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dustin/gomemcached"
	"github.com/dustin/gomemcached/client"

	"github.com/couchbaselabs/cbfs/config"
)

var verifyWorkers = flag.Int("verifyWorkers", 4,
	"Number of object verification workers.")
var maxStartupObjects = flag.Int("maxStartObjs", 1000,
	"Maximum number of objects to pull on start")
var maxStartupRepls = flag.Int("maxStartRepls", 3,
	"Blob replication limit for startup objects.")

var noFSFree = errors.New("no filesystemFree")

type PeriodicJob struct {
	period func() time.Duration
	f      func() error
}

var periodicJobs = map[string]*PeriodicJob{
	"checkStaleNodes": &PeriodicJob{
		func() time.Duration {
			return globalConfig.StaleNodeCheckFreq
		},
		checkStaleNodes,
	},
	"garbageCollectBlobs": &PeriodicJob{
		func() time.Duration {
			return globalConfig.GCFreq
		},
		garbageCollectBlobs,
	},
	"ensureMinReplCount": &PeriodicJob{
		func() time.Duration {
			return globalConfig.UnderReplicaCheckFreq
		},
		ensureMinimumReplicaCount,
	},
	"pruneExcessiveReplicas": &PeriodicJob{
		func() time.Duration {
			return globalConfig.OverReplicaCheckFreq
		},
		pruneExcessiveReplicas,
	},
}

type JobMarker struct {
	Node    string    `json:"node"`
	Started time.Time `json:"started"`
	Type    string    `json:"type"`
}

// Run a named task if we know one hasn't in the last t seconds.
func runNamedGlobalTask(name string, t time.Duration, f func() error) bool {
	key := "/@" + name

	if t.Seconds() < 1 {
		log.Printf("WARN: would've run with a 0s ttl, skipping %v",
			name)
		time.Sleep(time.Second)
		return false
	}

	jm := JobMarker{
		Node:    serverId,
		Started: time.Now(),
		Type:    "job",
	}

	err := couchbase.Do(key, func(mc *memcached.Client, vb uint16) error {
		resp, err := mc.Add(vb, key, 0, int(t.Seconds()),
			mustEncode(&jm))
		if err != nil {
			return err
		}
		if resp.Status != gomemcached.SUCCESS {
			return fmt.Errorf("Wanted success, got %v", resp.Status)
		}
		return nil
	})

	if err == nil {
		err = f()
		if err != nil {
			log.Printf("Error running periodic task %#v: %v", name, err)
		}
		return true
	}

	return false
}

func heartbeat() {
	for {
		u, err := url.Parse(*couchbaseServer)
		c, err := net.Dial("tcp", u.Host)
		localAddr := ""
		if err == nil {
			localAddr = strings.Split(c.LocalAddr().String(), ":")[0]
			c.Close()
		}

		freeSpace, err := filesystemFree()
		if err != nil && err != noFSFree {
			log.Printf("Error getting filesystem info: %v", err)
		}

		if maxStorage > 0 && freeSpace > maxStorage {
			freeSpace = maxStorage
		}

		aboutMe := StorageNode{
			Addr:     localAddr,
			Type:     "node",
			Time:     time.Now().UTC(),
			BindAddr: *bindAddr,
			Used:     spaceUsed,
			Free:     freeSpace,
		}

		err = couchbase.Set("/"+serverId, aboutMe)
		if err != nil {
			log.Printf("Failed to record a heartbeat: %v", err)
		}
		time.Sleep(globalConfig.HeartbeatFreq)
	}
}

func reconcileLoop() {
	if globalConfig.ReconcileFreq == 0 {
		return
	}
	for {
		err := reconcile()
		if err != nil {
			log.Printf("Error in reconciliation loop: %v", err)
		}
		grabSomeData()
		time.Sleep(globalConfig.ReconcileFreq)
	}
}

func salvageBlob(oid, deadNode string, nl NodeList) {
	candidates := nl.candidatesFor(oid,
		NodeList{nl.named(deadNode)})

	if len(candidates) == 0 {
		log.Printf("Couldn't find a candidate for blob!")
	} else {
		queueBlobAcquire(candidates[0], oid)
	}
}

func cleanupNode(node string) {
	nodes, err := findAllNodes()
	if err != nil {
		log.Printf("Error finding node list, aborting clean: %v", err)
		return
	}

	log.Printf("Cleaning up node %v", node)
	vres, err := couchbase.View("cbfs", "node_blobs",
		map[string]interface{}{
			"key":    `"` + node + `"`,
			"limit":  globalConfig.NodeCleanCount,
			"reduce": false,
			"stale":  false,
		})
	if err != nil {
		log.Printf("Error executing node_blobs view: %v", err)
		return
	}
	foundRows := 0
	for _, r := range vres.Rows {
		numOwners := removeBlobOwnershipRecord(r.ID[1:], node)
		foundRows++

		if numOwners < globalConfig.MinReplicas {
			salvageBlob(r.ID[1:], node, nodes)
		}
	}
	if foundRows == 0 && len(vres.Errors) == 0 {
		log.Printf("Removing node record: %v", node)
		err = couchbase.Delete("/" + node)
		if err != nil {
			log.Printf("Error deleting %v node record: %v", node, err)
		}
		err = couchbase.Delete("/" + node + "/r")
		if err != nil {
			log.Printf("Error deleting %v node counter: %v", node, err)
		}
	}
}

func checkStaleNodes() error {
	log.Printf("Checking stale nodes")
	nl, err := findAllNodes()
	if err != nil {
		return err
	}

	for _, node := range nl {
		d := time.Since(node.Time)

		if d > globalConfig.StaleNodeLimit {
			if node.IsLocal() {
				log.Printf("Would've cleaned up myself after %v",
					d)
				continue
			}
			log.Printf("  Node %v missed heartbeat schedule: %v",
				node.name, d)
			go cleanupNode(node.name)
		} else {
			log.Printf("%v is ok at %v", node.name, d)
		}
	}
	return nil
}

func garbageCollectBlobs() error {
	log.Printf("Garbage collecting blobs without any file references")

	viewRes := struct {
		Rows []struct {
			Key []string
		}
		Errors []struct {
			From   string
			Reason string
		}
	}{}

	// we hit this view descending because we want file sorted before blob
	// the fact that we walk the list backwards hopefully not too awkward
	err := couchbase.ViewCustom("cbfs", "file_blobs",
		map[string]interface{}{
			"stale":      false,
			"descending": true,
			"limit":      globalConfig.GCLimit,
		}, &viewRes)
	if err != nil {
		return err
	}

	if len(viewRes.Errors) > 0 {
		return fmt.Errorf("View errors: %v", viewRes.Errors)
	}

	nm, err := findNodeMap()
	if err != nil {
		return err
	}

	lastBlob := ""
	count := 0
	for _, r := range viewRes.Rows {
		blobId := r.Key[0]
		typeFlag := r.Key[1]
		blobNode := r.Key[2]

		switch typeFlag {
		case "file":
			lastBlob = blobId
		case "blob":
			if blobId != lastBlob {
				n, ok := nm[blobNode]
				if ok {
					queueBlobRemoval(n, blobId)
					count++
				} else {
					log.Printf("No nodemap entry for %v",
						blobNode)
				}
			}
		}

	}
	log.Printf("Scheduled %d blobs for deletion", count)
	return nil
}

func removeBlobFromNode(oid string, node StorageNode) {
	if node.name == serverId {
		//local delete
		err := removeObject(oid)
		if err != nil {
			log.Printf("Error removing blob, already deleted? %v", err)
		}
	} else {
		//remote
		err := node.deleteBlob(oid)
		if err != nil {
			log.Printf("Error GCing blob: %v", err)
			return
		}
	}
	log.Printf("Removed blob: %v from node %v", oid, node.name)
}

type fetchSpec struct {
	oid  string
	node string
}

func dataInitFetchOne(h, u string) error {
	f, err := NewHashRecord(*root, h)
	if err != nil {
		return err
	}
	defer f.Close()

	resp, err := http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		io.Copy(ioutil.Discard, resp.Body)
		return fmt.Errorf("Unexpected status fetching %v from %v: %v",
			h, u, resp.Status)
	}

	h, l, err := f.Process(resp.Body)
	if err != nil {
		return err
	}
	return recordBlobOwnership(h, l)
}

func dataInitFetcher(nm map[string]StorageNode, ch <-chan fetchSpec) {
	for fs := range ch {
		node, found := nm[fs.node]
		if !found {
			log.Printf("couldn't find %v", fs.node)
			continue
		}
		log.Printf("Fetching %v from %v", fs.oid, node.BlobURL(fs.oid))
		err := dataInitFetchOne(fs.oid, node.BlobURL(fs.oid))
		if err != nil {
			log.Printf("Error fetching %v: %v", fs.oid, err)
		}
	}
}

func grabSomeData() {
	viewRes := struct {
		Rows []struct {
			Id  string
			Doc struct {
				Json struct {
					Nodes map[string]string
				}
			}
		}
	}{}

	// Find some less replicated docs to suck in.
	err := couchbase.ViewCustom("cbfs", "repcounts",
		map[string]interface{}{
			"reduce":       false,
			"include_docs": true,
			"limit":        *maxStartupObjects,
			"startkey":     1,
			"endkey":       *maxStartupRepls - 1,
			"stale":        false,
		},
		&viewRes)

	if err != nil {
		log.Printf("Error finding docs to suck: %v", err)
		return
	}

	nl, err := findRemoteNodes()
	if err != nil {
		log.Printf("Error finding nodes: %v", err)
		return
	}
	nm := map[string]StorageNode{}

	for _, n := range nl {
		nm[n.name] = n
	}

	ch := make(chan fetchSpec, 1000)
	defer close(ch)

	for i := 0; i < 4; i++ {
		go dataInitFetcher(nm, ch)
	}

	for _, r := range viewRes.Rows {
		if _, ok := r.Doc.Json.Nodes[serverId]; !ok {
			for n := range r.Doc.Json.Nodes {
				if n != serverId {
					ch <- fetchSpec{r.Id[1:], n}
				}
			}
		}
	}
}

func runPeriodicJob(name string, job *PeriodicJob) {
	time.Sleep(time.Second * time.Duration(5+rand.Intn(60)))
	for {
		if runNamedGlobalTask(name, job.period(), job.f) {
			log.Printf("Attempted job %v", name)
		} else {
			log.Printf("Didn't run job %v", name)
		}
		time.Sleep(job.period() + time.Second)
	}
}

func runPeriodicJobs() {
	for n, j := range periodicJobs {
		go runPeriodicJob(n, j)
	}
}

func updateConfig() error {
	conf := cbfsconfig.CBFSConfig{}
	err := conf.RetrieveConfig(couchbase)
	if err != nil {
		return err
	}
	globalConfig = &conf
	return nil
}

func reloadConfig() {
	for {
		time.Sleep(time.Minute)
		if err := updateConfig(); err != nil {
			log.Printf("Error updating config: %v", err)
		}
	}
}
