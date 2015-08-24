/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"

	"github.com/gogo/protobuf/proto"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/auth"
	"github.com/mesos/mesos-go/auth/sasl"
	"github.com/mesos/mesos-go/auth/sasl/mech"
	mesos "github.com/mesos/mesos-go/mesosproto"
	"github.com/mesos/mesos-go/scheduler"
	"github.com/samuel/go-zookeeper/zk"
	"golang.org/x/net/context"

	"github.com/mesosphere/etcd-mesos/rpc"
	etcdscheduler "github.com/mesosphere/etcd-mesos/scheduler"
)

func parseIP(address string) net.IP {
	addr, err := net.LookupIP(address)
	if err != nil {
		log.Fatal(err)
	}
	if len(addr) < 1 {
		log.Fatalf("failed to parse IP from address '%v'", address)
	}
	return addr[0]
}

func main() {
	clusterName :=
		flag.String("cluster-name", "default", "Unique name of this etcd cluster")
	master :=
		flag.String("master", "127.0.0.1:5050", "Master address <ip:port>")
	zkFrameworkPersist :=
		flag.String("zk-framework-persist", "", "Zookeeper URI of the form zk://host1:port1,host2:port2/chroot/path")
	taskCount :=
		flag.Int("cluster-size", 5, "Total task count to run")
	adminPort :=
		flag.Int("admin-port", 23400, "Binding port for admin interface")
	reseedTimeout :=
		flag.Int("reseed-timeout", 240, "Seconds of etcd livelock to wait for before attempting a cluster re-seed")
	autoReseed :=
		flag.Bool("auto-reseed", true, "Perform automatic cluster reseed when the "+
			"cluster has been livelocked for -reseed-timeout seconds")
	artifactPort :=
		flag.Int("artifact-port", 12300, "Binding port for artifact server")
	executorPath :=
		flag.String("executor-bin", "./bin/etcd-mesos-executor", "Path to executor binary")
	etcdPath :=
		flag.String("etcd-bin", "./bin/etcd", "Path to etcd binary")
	address :=
		flag.String("address", "", "Binding address for scheduler and artifact server")
	driverPort :=
		flag.Int("driver-port", 0, "Binding port for scheduler driver")
	mesosAuthPrincipal :=
		flag.String("mesos-authentication-principal", "", "Mesos authentication principal")
	mesosAuthSecretFile :=
		flag.String("mesos-authentication-secret-file", "", "Mesos authentication secret file")
	authProvider :=
		flag.String("mesos-authentication-provider", sasl.ProviderName,
			fmt.Sprintf("Authentication provider to use, default is SASL that supports mechanisms: %+v", mech.ListSupported()))
	singleInstancePerSlave :=
		flag.Bool("single-instance-per-slave", true, "Only allow one etcd instance to be started per slave")
	testMode :=
		flag.Bool("test-mode", false, "disable forced values for -single-instance-per-slave and "+
			"-zk-framework-persist")
	failoverTimeoutSeconds :=
		flag.Float64("failover-timeout-seconds", 60*60*24*7, "Mesos framework failover timeout in seconds")
	flag.Parse()

	if *zkFrameworkPersist == "" && !*testMode {
		log.Fatal("No value provided for -zk-framework-persist ! This can be " +
			"overridden by experts using the -test-mode=true argument, but several " +
			"runtime guarantees no longer hold, and all tasks will be orphaned when " +
			"this process exits.")
	}

	if !*singleInstancePerSlave && !*testMode {
		log.Fatal("-single-instance-per-slave=false is dangerous because it may lead to " +
			"multiple etcd instances in the same cluster on a single node, amplifying " +
			"the cost of a single node being lost, livelock, and data loss.  This can " +
			"be overridden by passing the -test-mode=true argument, at your peril.")
	}

	if *address == "" {
		hostname, err := os.Hostname()
		if err == nil {
			*address = hostname
		} else {
			log.Errorf("Could not set default binding to hostname.  Defaulting to 127.0.0.1")
			*address = "127.0.0.1"
		}
	}

	executorUris := []*mesos.CommandInfo_URI{}
	execUri, err := etcdscheduler.ServeExecutorArtifact(*executorPath, *address, *artifactPort)
	if err != nil {
		log.Errorf("Could not stat executor binary: %v", err)
		return
	}
	executorUris = append(executorUris, &mesos.CommandInfo_URI{
		Value:      execUri,
		Executable: proto.Bool(true),
	})
	etcdUri, err := etcdscheduler.ServeExecutorArtifact(*etcdPath, *address, *artifactPort)
	if err != nil {
		log.Errorf("Could not stat etcd binary: %v", err)
		return
	}
	executorUris = append(executorUris, &mesos.CommandInfo_URI{
		Value:      etcdUri,
		Executable: proto.Bool(true),
	})

	go http.ListenAndServe(fmt.Sprintf("%s:%d", *address, *artifactPort), nil)
	log.V(2).Info("Serving executor artifacts...")

	bindingAddress := parseIP(*address)

	// chillFactor is the number of seconds that are slept for to allow for
	// convergence across the cluster during mutations.
	chillFactor := 10
	etcdScheduler := etcdscheduler.NewEtcdScheduler(
		*taskCount,
		chillFactor,
		*reseedTimeout,
		*autoReseed,
		executorUris,
		*singleInstancePerSlave,
	)
	etcdScheduler.ExecutorPath = *executorPath
	etcdScheduler.Master = *master
	etcdScheduler.ClusterName = *clusterName
	etcdScheduler.ZkConnect = *zkFrameworkPersist

	fwinfo := &mesos.FrameworkInfo{
		User:            proto.String(""), // Mesos-go will fill in user.
		Name:            proto.String("etcd-" + etcdScheduler.ClusterName),
		Checkpoint:      proto.Bool(true),
		FailoverTimeout: proto.Float64(*failoverTimeoutSeconds),
	}

	cred := (*mesos.Credential)(nil)
	if *mesosAuthPrincipal != "" {
		fwinfo.Principal = proto.String(*mesosAuthPrincipal)
		secret, err := ioutil.ReadFile(*mesosAuthSecretFile)
		if err != nil {
			log.Fatal(err)
		}
		cred = &mesos.Credential{
			Principal: proto.String(*mesosAuthPrincipal),
			Secret:    secret,
		}
	}

	zkServers, zkChroot, err := rpc.ParseZKURI(*zkFrameworkPersist)
	etcdScheduler.ZkServers = zkServers
	etcdScheduler.ZkChroot = zkChroot
	if err != nil && *zkFrameworkPersist != "" {
		log.Fatalf("Error parsing zookeeper URI of %s: %s", *zkFrameworkPersist, err)
	} else if *zkFrameworkPersist != "" {
		previous, err := rpc.GetPreviousFrameworkID(
			zkServers,
			zkChroot,
			etcdScheduler.ClusterName,
		)
		if err != nil && err != zk.ErrNoNode {
			log.Fatalf("Could not retrieve previous framework ID: %s", err)
		} else if err == zk.ErrNoNode {
			log.Info("No previous persisted framework ID exists in zookeeper.")
		} else {
			log.Infof("Found stored framework ID in Zookeeper, "+
				"attempting to re-use: %s", previous)
			fwinfo.Id = &mesos.FrameworkID{
				Value: proto.String(previous),
			}
		}
	}

	config := scheduler.DriverConfig{
		Scheduler:      etcdScheduler,
		Framework:      fwinfo,
		Master:         etcdScheduler.Master,
		Credential:     cred,
		BindingAddress: bindingAddress,
		BindingPort:    uint16(*driverPort),
		WithAuthContext: func(ctx context.Context) context.Context {
			ctx = auth.WithLoginProvider(ctx, *authProvider)
			ctx = sasl.WithBindingAddress(ctx, bindingAddress)
			return ctx
		},
	}

	driver, err := scheduler.NewMesosSchedulerDriver(config)

	if err != nil {
		log.Errorln("Unable to create a SchedulerDriver ", err.Error())
	}

	go etcdScheduler.SerialLauncher(driver)
	go etcdScheduler.PeriodicLaunchRequestor()
	go etcdScheduler.AdminHTTP(*adminPort, driver)

	if stat, err := driver.Run(); err != nil {
		log.Infof("Framework stopped with status %s and error: %s",
			stat.String(),
			err.Error())
	}
}