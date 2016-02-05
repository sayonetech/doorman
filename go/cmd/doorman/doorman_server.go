// Copyright 2016 Google, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	_ "expvar"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/context"

	log "github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	rpc "google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/youtube/doorman/go/connection"
	"github.com/youtube/doorman/go/server/doorman"
	"github.com/youtube/doorman/go/server/election"
	"github.com/youtube/doorman/go/status"

	pb "github.com/youtube/doorman/proto/doorman"
)

var (
	port = flag.Int("port", 0, "port to bind to")
	// FIXME(ryszard): As of Jan 21, 2016 it's impossible to serve
	// both RPC and HTTP traffic on the same port. This should be
	// fixed by grpc/grpc-go#75. When that happens, remove
	// debugPort.
	debugPort  = flag.Int("debug_port", 8081, "port to bind for HTTP debug info")
	serverRole = flag.String("server_role", "root", "Role of this server in the server tree")
	parent     = flag.String("parent", "", "Address of the parent server which this server connects to")

	config = flag.String("config", "", "file to load the config from (text protobufs)")

	rpcDialTimeout = flag.Duration("doorman_rpc_dial_timeout", 5*time.Second, "timeout to use for connecting to the doorman server")

	minimumRefreshInterval = flag.Duration("doorman_minimum_refresh_interval", 5*time.Second, "minimum refresh interval")

	tls      = flag.Bool("tls", false, "Connection uses TLS if true, else plain TCP")
	certFile = flag.String("cert_file", "", "The TLS cert file")
	keyFile  = flag.String("key_file", "", "The TLS key file")

	etcdEndpoints      = flag.String("etcd_endpoints", "", "comma separated list of etcd endpoints")
	masterDelay        = flag.Duration("master_delay", 10*time.Second, "delay in master elections")
	masterElectionLock = flag.String("master_election_lock", "", "etcd path for the master election or empty for no master election")
)

var (
	statusz = `
<h2>Mastership</h2>
<p>
{{if .IsMaster}}
  This <strong>is</strong> the master.
{{else}}
This is <strong>not</strong> the master.
  {{with .CurrentMaster}}
    The current master is <a href="http://{{.}}">{{.}}</a>
  {{else}}
    The current master is unknown.
  {{end}}
{{end}}
</p>
{{with .Election}}{{.}}{{end}}

<h2>Resources</h2>
{{ with .Resources }}
<table border="1">
  <thead>
    <tr>
      <td>ID</td>
      <td>Capacity</td>
      <td>SumHas</td>
      <td>SumWants</td>
      <td>Clients</td>
      <td>Learning</td>
      <td>Algorithm</td>
    </tr>
  </thead>
  {{range .}}
  <tr>
    <td><a href="/debug/resources?resource={{.ID}}">{{.ID}}</a></td>
    <td>{{.Capacity}}</td>
    <td>{{.SumHas}}</td>
    <td>{{.SumWants}}</td>
    <td>{{.Count}}</td>
    <td>{{.InLearningMode}}
    <td><code>{{.Algorithm}}</code></td>
  </tr>
  {{end}}
</table>
{{else}}
No resources in the store.
{{end}}

<h2>Configuration</h2>
<pre>{{.Config}}</pre>
`
)

// getServerID returns a unique server id, consisting of a host:pid id.
func getServerID(port int) string {

	hostname, err := os.Hostname()

	if err != nil {
		hostname = "unknown.localhost"
	}

	return fmt.Sprintf("%s:%d", hostname, port)
}

func main() {
	flag.Parse()

	if *config == "" {
		log.Exit("--config cannot be empty")
	}

	var masterElection election.Election
	if *masterElectionLock != "" {
		etcdEndpointsSlice := strings.Split(*etcdEndpoints, ",")
		if len(etcdEndpointsSlice) == 1 && etcdEndpointsSlice[0] == "" {
			log.Exit("-etcd_endpoints cannot be empty if -master_election_lock is provided")
		}

		masterElection = election.Etcd(etcdEndpointsSlice, *masterElectionLock, *masterDelay)
	} else {
		masterElection = election.Trivial()
	}

	dm, err := doorman.NewIntermediate(context.Background(), getServerID(*port), *parent, masterElection,
		connection.MinimumRefreshInterval(*minimumRefreshInterval),
		connection.DialOpts(
			rpc.WithTimeout(*rpcDialTimeout)))
	if err != nil {
		log.Exitf("doorman.NewIntermediate: %v", err)
	}

	var opts []rpc.ServerOption
	if *tls {
		log.Infof("Loading credentials from %v and %v.", *certFile, *keyFile)
		creds, err := credentials.NewServerTLSFromFile(*certFile, *keyFile)
		if err != nil {
			log.Exitf("Failed to generate credentials %v", err)
		}
		opts = []rpc.ServerOption{rpc.Creds(creds)}
	}
	server := rpc.NewServer(opts...)

	pb.RegisterCapacityServer(server, dm)

	if *config == "" {
		log.Exit("-config cannot be empty")
	}

	data, err := ioutil.ReadFile(*config)
	if err != nil {
		log.Exitf("cannot read config file: %v", err)
	}

	cfg := new(pb.ResourceRepository)
	if err := proto.UnmarshalText(string(data), cfg); err != nil {
		log.Exitf("cannot load config: %v", err)
	}

	if err := dm.LoadConfig(context.Background(), cfg, map[string]*time.Time{}); err != nil {
		log.Exitf("dm.LoadConfig: %v", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)

	go func() {
		for range c {
			log.Infof("Received SIGHUP, attempting to reload configuration from %v.", *config)
			data, err := ioutil.ReadFile(*config)
			if err != nil {
				log.Errorf("cannot read config file: %v", err)
				continue
			}

			cfg := new(pb.ResourceRepository)
			if err := proto.UnmarshalText(string(data), cfg); err != nil {
				log.Errorf("cannot load config: %v", err)
				continue
			}

			if err := dm.LoadConfig(context.Background(), cfg, map[string]*time.Time{}); err != nil {
				log.Errorf("dm.LoadConfig: %v", err)
			}
			log.Infof("Reloaded config from %v", *config)
		}
	}()

	// FIXME(ryszard): Add some sort of statusz capabilities.
	status.AddStatusPart("Doorman", statusz, func(context.Context) interface{} { return dm.Status() })
	// // Adds this new Doorman server for /statusz and /resourcez.
	// server.AddStatuszPart("Doorman", statusz, func(context.Context) interface{} { return dm.Status() })
	AddServer(dm)

	go http.ListenAndServe(fmt.Sprintf(":%v", *debugPort), nil)

	// Waits for the server to get its initial configuration. This guarantees that
	// the server will never run without a valid configuration.
	log.Info("Waiting for the server to be configured...")
	dm.WaitUntilConfigured()

	// Runs the server.
	log.Info("Server is configured, ready to go!")

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Exit(err)
	}

	server.Serve(lis)

}