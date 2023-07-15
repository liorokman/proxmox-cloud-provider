package main

import (
	"fmt"
	"log"
	"net"
	"os"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
	"google.golang.org/grpc"
)

var k = koanf.New(".")

func init() {
	log.Printf("Loading configuration...")
	somethingLoaded := false
	err := loadConfigFile("/etc/lbmanager/lbmanager.yaml")
	if err == nil {
		somethingLoaded = true
	} else if !os.IsNotExist(err) {
		log.Fatalf("error loading config: %v", err)
	}

	err = loadConfigFile("lbmanager.yaml")
	if err == nil {
		somethingLoaded = true
	} else if !os.IsNotExist(err) {
		log.Fatalf("error loading local config: %v", err)
	}

	if !somethingLoaded {
		log.Fatalf("No configuration found. Cowardly refusing to continue.\n")
	}
	log.Printf("done\n")
}

func loadConfigFile(configFile string) error {
	mainConfigFile := file.Provider(configFile)
	if err := k.Load(mainConfigFile, yaml.Parser()); err != nil {
		return err
	}
	mainConfigFile.Watch(func(event any, err error) {
		if err != nil {
			log.Printf("watch error: %v\n", err)
			return
		}
		log.Println("config changed, reloading ...")
		tmpK := koanf.New(".")
		if err := tmpK.Load(mainConfigFile, yaml.Parser()); err != nil {
			log.Printf("error loading the new config: %v\n", err)
			return
		}
		k.Merge(tmpK)
	})
	return nil
}

func main() {

	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", k.MustString("grpc.listen"), k.MustInt("grpc.port")))
	if err != nil {
		log.Fatalf("failed listening: %v", err)
	}
	lbServer, err := loadbalancer.NewServer(k)
	if err != nil {
		log.Fatalf("failed starting the loadbalancer manager: %v", err)
	}
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	loadbalancer.RegisterLoadBalancerServer(grpcServer, lbServer)
	log.Println("Listening...")
	grpcServer.Serve(lis)
}
