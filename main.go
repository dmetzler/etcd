package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/coreos/etcd/discovery"
	"github.com/coreos/etcd/etcdserver"
	"github.com/coreos/etcd/etcdserver/etcdhttp"
	"github.com/coreos/etcd/pkg"
	flagtypes "github.com/coreos/etcd/pkg/flags"
	"github.com/coreos/etcd/pkg/transport"
	"github.com/coreos/etcd/proxy"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/snap"
	"github.com/coreos/etcd/store"
	"github.com/coreos/etcd/wal"
)

const (
	// the owner can make/remove files inside the directory
	privateDirMode = 0700

	version = "0.5.0-alpha"
)

var (
	name         = flag.String("name", "default", "Unique human-readable name for this node")
	timeout      = flag.Duration("timeout", 10*time.Second, "Request Timeout")
	paddr        = flag.String("peer-bind-addr", ":7001", "Peer service address (e.g., ':7001')")
	dir          = flag.String("data-dir", "", "Path to the data directory")
	durl         = flag.String("discovery", "", "Discovery service used to bootstrap the cluster")
	purls        = flag.String("advertised-peer-urls", "", "Comma-separated public urls used for peer communication")
	snapCount    = flag.Int64("snapshot-count", etcdserver.DefaultSnapCount, "Number of committed transactions to trigger a snapshot")
	printVersion = flag.Bool("version", false, "Print the version and exit")

	cluster   = &etcdserver.Cluster{}
	addrs     = &flagtypes.Addrs{}
	cors      = &pkg.CORSInfo{}
	proxyFlag = new(flagtypes.Proxy)

	clientTLSInfo = transport.TLSInfo{}
	peerTLSInfo   = transport.TLSInfo{}

	deprecated = []string{
		"addr",
		"cluster-active-size",
		"cluster-remove-delay",
		"cluster-sync-interval",
		"config",
		"force",
		"max-result-buffer",
		"max-retry-attempts",
		"peer-addr",
		"peer-heartbeat-interval",
		"peer-election-timeout",
		"retry-interval",
		"snapshot",
		"v",
		"vv",
	}
)

func init() {
	flag.Var(cluster, "bootstrap-config", "Initial cluster configuration for bootstrapping")
	flag.Var(addrs, "bind-addr", "List of HTTP service addresses (e.g., '127.0.0.1:4001,10.0.0.1:8080')")
	flag.Var(cors, "cors", "Comma-separated white list of origins for CORS (cross-origin resource sharing).")
	flag.Var(proxyFlag, "proxy", fmt.Sprintf("Valid values include %s", strings.Join(flagtypes.ProxyValues, ", ")))
	cluster.Set("default=localhost:8080")
	addrs.Set("127.0.0.1:4001")
	proxyFlag.Set(flagtypes.ProxyValueOff)

	flag.StringVar(&clientTLSInfo.CAFile, "ca-file", "", "Path to the client server TLS CA file.")
	flag.StringVar(&clientTLSInfo.CertFile, "cert-file", "", "Path to the client server TLS cert file.")
	flag.StringVar(&clientTLSInfo.KeyFile, "key-file", "", "Path to the client server TLS key file.")

	flag.StringVar(&peerTLSInfo.CAFile, "peer-ca-file", "", "Path to the peer server TLS CA file.")
	flag.StringVar(&peerTLSInfo.CertFile, "peer-cert-file", "", "Path to the peer server TLS cert file.")
	flag.StringVar(&peerTLSInfo.KeyFile, "peer-key-file", "", "Path to the peer server TLS key file.")

	for _, f := range deprecated {
		flag.Var(&pkg.DeprecatedFlag{f}, f, "")
	}
}

func main() {
	flag.Usage = pkg.UsageWithIgnoredFlagsFunc(flag.CommandLine, deprecated)
	flag.Parse()

	if *printVersion {
		fmt.Println("etcd version", version)
		os.Exit(0)
	}

	pkg.SetFlagsFromEnv(flag.CommandLine)

	if string(*proxyFlag) == flagtypes.ProxyValueOff {
		startEtcd()
	} else {
		startProxy()
	}

	// Block indefinitely
	<-make(chan struct{})
}

// startEtcd launches the etcd server and HTTP handlers for client/server communication.
func startEtcd() {
	self := cluster.FindName(*name)
	if self == nil {
		log.Fatalf("etcd: no member with name=%q exists", *name)
	}

	if self.ID == raft.None {
		log.Fatalf("etcd: cannot use None(%d) as member id", raft.None)
	}

	if *snapCount <= 0 {
		log.Fatalf("etcd: snapshot-count must be greater than 0: snapshot-count=%d", *snapCount)
	}

	if *dir == "" {
		*dir = fmt.Sprintf("%v_etcd_data", self.ID)
		log.Printf("main: no data-dir is given, using default data-dir ./%s", *dir)
	}
	if err := os.MkdirAll(*dir, privateDirMode); err != nil {
		log.Fatalf("main: cannot create data directory: %v", err)
	}
	snapdir := path.Join(*dir, "snap")
	if err := os.MkdirAll(snapdir, privateDirMode); err != nil {
		log.Fatalf("etcd: cannot create snapshot directory: %v", err)
	}
	snapshotter := snap.New(snapdir)

	waldir := path.Join(*dir, "wal")
	var w *wal.WAL
	var n raft.Node
	var err error
	st := store.New()

	if !wal.Exist(waldir) {
		if *durl != "" {
			if *purls == "" {
				log.Fatal("etcd: discovery requires advertised-peer-urls")
			}
			cfg := fmt.Sprintf("%s=%s", *name, *purls)
			d, err := discovery.New(*durl, self.ID, cfg)
			if err != nil {
				log.Fatalf("etcd: cannot init discovery %v", err)
			}
			cluster, err = d.Discover()
			if err != nil {
				log.Fatalf("etcd: %v", err)
			}
		}
		w, err = wal.Create(waldir)
		if err != nil {
			log.Fatal(err)
		}
		n = raft.StartNode(self.ID, cluster.IDs(), 10, 1)
	} else {
		var index int64
		snapshot, err := snapshotter.Load()
		if err != nil && err != snap.ErrNoSnapshot {
			log.Fatal(err)
		}
		if snapshot != nil {
			log.Printf("etcd: restart from snapshot at index %d", snapshot.Index)
			st.Recovery(snapshot.Data)
			index = snapshot.Index
		}

		// restart a node from previous wal
		if w, err = wal.OpenAtIndex(waldir, index); err != nil {
			log.Fatal(err)
		}
		wid, st, ents, err := w.ReadAll()
		if err != nil {
			log.Fatal(err)
		}
		// TODO(xiangli): save/recovery nodeID?
		if wid != 0 {
			log.Fatalf("unexpected nodeid %d: nodeid should always be zero until we save nodeid into wal", wid)
		}
		n = raft.RestartNode(self.ID, cluster.IDs(), 10, 1, snapshot, st, ents)
	}

	pt, err := transport.NewTransport(peerTLSInfo)
	if err != nil {
		log.Fatal(err)
	}

	cls := etcdserver.NewClusterStore(st, *cluster)

	s := &etcdserver.EtcdServer{
		Store: st,
		Node:  n,
		Storage: struct {
			*wal.WAL
			*snap.Snapshotter
		}{w, snapshotter},
		Send:         etcdserver.Sender(pt, cls),
		Ticker:       time.Tick(100 * time.Millisecond),
		SyncTicker:   time.Tick(500 * time.Millisecond),
		SnapCount:    *snapCount,
		ClusterStore: cls,
	}
	s.Start()

	ch := &pkg.CORSHandler{
		Handler: etcdhttp.NewClientHandler(s, cls, *timeout),
		Info:    cors,
	}
	ph := etcdhttp.NewPeerHandler(s)

	l, err := transport.NewListener(*paddr, peerTLSInfo)
	if err != nil {
		log.Fatal(err)
	}

	// Start the peer server in a goroutine
	go func() {
		log.Print("Listening for peers on ", *paddr)
		log.Fatal(http.Serve(l, ph))
	}()

	// Start a client server goroutine for each listen address
	for _, addr := range *addrs {
		addr := addr
		l, err := transport.NewListener(addr, clientTLSInfo)
		if err != nil {
			log.Fatal(err)
		}

		go func() {
			log.Print("Listening for client requests on ", addr)
			log.Fatal(http.Serve(l, ch))
		}()
	}
}

// startProxy launches an HTTP proxy for client communication which proxies to other etcd nodes.
func startProxy() {
	pt, err := transport.NewTransport(clientTLSInfo)
	if err != nil {
		log.Fatal(err)
	}

	ph, err := proxy.NewHandler(pt, (*cluster).PeerURLs())
	if err != nil {
		log.Fatal(err)
	}

	ph = &pkg.CORSHandler{
		Handler: ph,
		Info:    cors,
	}

	if string(*proxyFlag) == flagtypes.ProxyValueReadonly {
		ph = proxy.NewReadonlyHandler(ph)
	}

	// Start a proxy server goroutine for each listen address
	for _, addr := range *addrs {
		addr := addr
		l, err := transport.NewListener(addr, clientTLSInfo)
		if err != nil {
			log.Fatal(err)
		}

		go func() {
			log.Print("Listening for client requests on ", addr)
			log.Fatal(http.Serve(l, ph))
		}()
	}
}
