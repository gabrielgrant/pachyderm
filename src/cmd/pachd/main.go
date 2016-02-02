package main

import (
	"fmt"
	"io/ioutil"

	"github.com/gengo/grpc-gateway/runtime"
	"github.com/pachyderm/pachyderm"
	"github.com/pachyderm/pachyderm/src/pfs"
	"github.com/pachyderm/pachyderm/src/pfs/drive"
	"github.com/pachyderm/pachyderm/src/pfs/route"
	"github.com/pachyderm/pachyderm/src/pfs/server"
	"github.com/pachyderm/pachyderm/src/pkg/discovery"
	"github.com/pachyderm/pachyderm/src/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/pkg/netutil"
	"github.com/pachyderm/pachyderm/src/pkg/obj"
	"github.com/pachyderm/pachyderm/src/pkg/shard"
	"github.com/pachyderm/pachyderm/src/pps"
	"github.com/pachyderm/pachyderm/src/pps/jobserver"
	"github.com/pachyderm/pachyderm/src/pps/persist"
	persistserver "github.com/pachyderm/pachyderm/src/pps/persist/server"
	"github.com/pachyderm/pachyderm/src/pps/pipelineserver"
	"go.pedge.io/env"
	"go.pedge.io/lion/proto"
	"go.pedge.io/pkg/http"
	"go.pedge.io/proto/server"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	kube "k8s.io/kubernetes/pkg/client/unversioned"
)

type appEnv struct {
	Port            uint16 `env:"PORT,default=650"`
	HTTPPort        uint16 `env:"HTTP_PORT,default=750"`
	NumShards       uint64 `env:"NUM_SHARDS,default=32"`
	StorageRoot     string `env:"PACH_ROOT,required"`
	DatabaseAddress string `env:"RETHINK_PORT_28015_TCP_ADDR,required"`
	DatabaseName    string `env:"DATABASE_NAME,default=pachyderm"`
	KubeAddress     string `env:"KUBERNETES_PORT_443_TCP_ADDR,required"`
	EtcdAddress     string `env:"ETCD_PORT_2379_TCP_ADDR,required"`
	Namespace       string `env:NAMESPACE,default=default"`
}

func main() {
	env.Main(do, &appEnv{})
}

func do(appEnvObj interface{}) error {
	appEnv := appEnvObj.(*appEnv)
	etcdClient := getEtcdClient(appEnv)
	rethinkAPIServer, err := getRethinkAPIServer(appEnv)
	if err != nil {
		return err
	}
	kubeClient, err := getKubeClient(appEnv)
	if err != nil {
		return err
	}
	address, err := netutil.ExternalIP()
	if err != nil {
		return err
	}
	address = fmt.Sprintf("%s:%d", address, appEnv.Port)
	sharder := shard.NewSharder(
		etcdClient,
		appEnv.NumShards,
		0,
		appEnv.Namespace,
	)
	go func() {
		if err := sharder.AssignRoles(nil); err != nil {
			protolion.Printf("Error from sharder.AssignRoles: %s", err.Error())
		}
	}()
	driver, err := drive.NewDriver(address)
	if err != nil {
		return err
	}
	apiServer := server.NewAPIServer(
		route.NewSharder(
			appEnv.NumShards,
			1,
		),
		route.NewRouter(
			sharder,
			grpcutil.NewDialer(
				grpc.WithInsecure(),
			),
			address,
		),
	)
	go func() {
		if err := sharder.RegisterFrontends(nil, address, []shard.Frontend{apiServer}); err != nil {
			protolion.Printf("Error from sharder.RegisterFrontend %s", err.Error())
		}
	}()
	internalAPIServer := server.NewInternalAPIServer(
		route.NewSharder(
			appEnv.NumShards,
			1,
		),
		route.NewRouter(
			sharder,
			grpcutil.NewDialer(
				grpc.WithInsecure(),
			),
			address,
		),
		driver,
	)
	go func() {
		if err := sharder.Register(nil, address, []shard.Server{internalAPIServer}); err != nil {
			protolion.Printf("Error from sharder.Register %s", err.Error())
		}
	}()
	jobAPIServer := jobserver.NewAPIServer(
		address,
		rethinkAPIServer,
		kubeClient,
	)
	jobAPIClient := pps.NewLocalJobAPIClient(jobAPIServer)
	pipelineAPIServer := pipelineserver.NewAPIServer(address, jobAPIClient, rethinkAPIServer)
	if err := pipelineAPIServer.Start(); err != nil {
		return err
	}
	var blockAPIServer pfs.BlockAPIServer
	if err := func() error {
		bucket, err := ioutil.ReadFile("/amazon-secret/bucket")
		if err != nil {
			return err
		}
		id, err := ioutil.ReadFile("/amazon-secret/id")
		if err != nil {
			return err
		}
		secret, err := ioutil.ReadFile("/amazon-secret/secret")
		if err != nil {
			return err
		}
		token, err := ioutil.ReadFile("/amazon-secret/token")
		if err != nil {
			return err
		}
		region, err := ioutil.ReadFile("/amazon-secret/region")
		if err != nil {
			return err
		}
		objClient, err := obj.NewAmazonClient(string(bucket), string(id), string(secret), string(token), string(region))
		if err != nil {
			return err
		}
		blockAPIServer, err = server.NewObjBlockAPIServer(appEnv.StorageRoot, objClient)
		if err != nil {
			return err
		}
		return nil
	}(); err != nil {
		protolion.Errorf("failed to create obj backend, falling back to local")
		blockAPIServer, err = server.NewLocalBlockAPIServer(appEnv.StorageRoot)
		if err != nil {
			return err
		}
	}
	return protoserver.ServeWithHTTP(
		func(s *grpc.Server) {
			pfs.RegisterAPIServer(s, apiServer)
			pfs.RegisterInternalAPIServer(s, internalAPIServer)
			pfs.RegisterBlockAPIServer(s, blockAPIServer)
			pps.RegisterJobAPIServer(s, jobAPIServer)
			pps.RegisterInternalJobAPIServer(s, jobAPIServer)
			pps.RegisterPipelineAPIServer(s, pipelineAPIServer)
		},
		func(ctx context.Context, mux *runtime.ServeMux, clientConn *grpc.ClientConn) error {
			return pfs.RegisterAPIHandler(ctx, mux, clientConn)
		},
		protoserver.ServeWithHTTPOptions{
			ServeOptions: protoserver.ServeOptions{
				Version: pachyderm.Version,
			},
		},
		protoserver.ServeEnv{
			GRPCPort: appEnv.Port,
		},
		pkghttp.HandlerEnv{
			Port: appEnv.HTTPPort,
		},
	)
}

func getEtcdClient(env *appEnv) discovery.Client {
	return discovery.NewEtcdClient(fmt.Sprintf("http://%s:2379", env.EtcdAddress))
}

func getKubeClient(env *appEnv) (*kube.Client, error) {
	kubeClient, err := kube.NewInCluster()
	if err != nil {
		protolion.Errorf("Falling back to insecure kube client due to error from NewInCluster: %s", err.Error())
	} else {
		return kubeClient, err
	}
	config := &kube.Config{
		Host:     fmt.Sprintf("%s:443", env.KubeAddress),
		Insecure: true,
	}
	return kube.New(config)
}

func getRethinkAPIServer(env *appEnv) (persist.APIServer, error) {
	if err := persistserver.InitDBs(fmt.Sprintf("%s:28015", env.DatabaseAddress), env.DatabaseName); err != nil {
		return nil, err
	}
	return persistserver.NewRethinkAPIServer(fmt.Sprintf("%s:28015", env.DatabaseAddress), env.DatabaseName)
}
