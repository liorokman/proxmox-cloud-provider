package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

var k = koanf.New(".")

var (
	clientCert = flag.String("cert", "", "filename containing the client certificate")
	clientKey  = flag.String("key", "", "filename containing the client certificate private key")
	caFile     = flag.String("ca", "", "filename containing the CA that can verify the server")

	serverAddr = flag.String("addr", "", "server address")
	name       = flag.String("name", "", "service name")
	op         = flag.String("op", "addSrv", "addSrv, delSrv, addTgt, delTgt, list")

	service  = flag.String("srv", "", "in {add,del}Srv - service ip, in {add,del}Tgt - target ip")
	protocol = flag.Bool("tcp", true, "true == TCP, false == UDP")
	dstPort  = flag.Int("dport", 8080, "destination port")
	srcPort  = flag.Int("sport", 8080, "source port")
)

func init() {
	err := loadConfigFile("/etc/lbmanager/lbctl.yaml")
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("failed loading config: %v", err)
	}
}

func loadConfigFile(configFile string) error {
	f := file.Provider(configFile)
	if err := k.Load(f, yaml.Parser()); err != nil {
		return err
	}
	return nil
}

func loadKeypair() (credentials.TransportCredentials, error) {
	if *clientKey == "" {
		*clientKey = k.String("auth.clientKey")
	}
	if *clientCert == "" {
		*clientCert = k.String("auth.clientCert")
	}
	if *clientKey == "" {
		clientKey = clientCert
	}
	if *caFile == "" {
		*caFile = k.String("auth.caFile")
	}
	if *clientKey == "" || *clientCert == "" || *caFile == "" {
		return nil, fmt.Errorf("no mTLS configuration found")
	}
	cert, err := tls.LoadX509KeyPair(*clientCert, *clientKey)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(*caFile)
	if err != nil {
		return nil, err
	}
	capool := x509.NewCertPool()
	if !capool.AppendCertsFromPEM(ca) {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      capool,
	}), nil
}

func main() {
	flag.Parse()

	if *name == "" && *op != "list" {
		log.Fatal("no loadbalancer name provided.")
	}

	creds, err := loadKeypair()
	if err != nil {
		log.Fatalf("error loading mTLS configuration: %+v", err)
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}
	if *serverAddr == "" {
		*serverAddr = k.String("addr")
	}
	conn, err := grpc.Dial(*serverAddr, opts...)
	if err != nil {
		log.Fatalf("error connecting to lbmanager: %+v", err)
	}
	defer conn.Close()

	var p loadbalancer.Protocol = loadbalancer.Protocol_TCP
	if !*protocol {
		p = loadbalancer.Protocol_UDP
	}

	client := loadbalancer.NewLoadBalancerClient(conn)

	switch *op {
	case "addSrv":
		clb := &loadbalancer.CreateLoadBalancer{
			Name:   *name,
			IpAddr: service,
		}
		lbInfo, err := client.Create(context.Background(), clb)
		if err != nil {
			log.Fatalf("Error during create: %s", err.Error())
		}
		log.Printf("LBInfo: %+v\n", lbInfo)

	case "delSrv":
		ret, err := client.Delete(context.Background(), &loadbalancer.LoadBalancerName{
			Name: *name,
		})
		if err != nil {
			log.Fatalf("Error during delete: %s", err.Error())
		}
		if len(ret.Message) != 0 {
			log.Printf("Message from server: %s", ret.Message)
		} else {
			log.Printf("Success!")
		}
	case "addTgt":
		ret, err := client.AddTarget(context.Background(), &loadbalancer.AddTargetRequest{
			LbName:  *name,
			SrcPort: int32(*srcPort),
			Target: &loadbalancer.Target{
				DstIP:    *service,
				DstPort:  int32(*dstPort),
				Protocol: p,
			},
		})
		if err != nil {
			log.Fatalf("Error adding destination: %s", err.Error())
		}
		if len(ret.Message) != 0 {
			log.Printf("Message from server: %s", ret.Message)
		} else {
			log.Printf("Success!")
		}
	case "delTgt":
		ret, err := client.DelTarget(context.Background(), &loadbalancer.DelTargetRequest{
			LbName:  *name,
			SrcPort: int32(*srcPort),
			Target: &loadbalancer.Target{
				DstIP:    *service,
				DstPort:  int32(*dstPort),
				Protocol: p,
			},
		})
		if err != nil {
			log.Fatalf("Error deleting destination: %s", err.Error())
		}
		if len(ret.Message) != 0 {
			log.Printf("Message from server: %s", ret.Message)
		} else {
			log.Printf("Success!")
		}

	case "list":
		stream, err := client.GetLoadBalancers(context.Background(), &emptypb.Empty{})
		if err != nil {
			log.Fatalf("Failed to list load balancers: %s", err.Error())
		}
		for {
			lb, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				log.Fatalf("Error listing load balancers %v", err)
			}
			log.Println(lb)
		}
	default:
		log.Fatalf("Unknown operation %s", *op)
	}
}
