package main

import (
	"context"
	"flag"
	"io"
	"log"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/liorokman/proxmox-cloud-provider/internal/loadbalancer"
)

var (
	serverAddr = flag.String("addr", "localhost:9999", "server address")
	name       = flag.String("name", "", "service name")
	op         = flag.String("op", "addSrv", "addSrv, delSrv, addTgt, delTgt, list")

	service  = flag.String("srv", "192.168.87.33:80", "in {add,del}Srv - service ip, in {add,del}Tgt - target ip")
	protocol = flag.Bool("tcp", true, "true == TCP, false == UDP")
	dstPort  = flag.Int("port", 0, "destination port")
)

func main() {
	flag.Parse()
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	conn, err := grpc.Dial(*serverAddr, opts...)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	var p loadbalancer.Protocol = loadbalancer.Protocol_TCP
	if !*protocol {
		p = loadbalancer.Protocol_UDP
	}

	client := loadbalancer.NewLoadBalancerClient(conn)

	switch *op {
	case "addSrv":
		parts := strings.Split(*service, ":")
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			log.Fatalf("malformed port: %s", parts[1])
		}
		clb := &loadbalancer.CreateLoadBalancer{
			Name:     *name,
			Port:     uint32(port),
			Protocol: p,
			IpAddr:   asPtr(parts[0]),
		}
		log.Printf("requested: %+v", clb)
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
			LbName: *name,
			Target: &loadbalancer.Target{
				DstIP:   *service,
				DstPort: uint32(*dstPort),
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
			LbName: *name,
			Target: &loadbalancer.Target{
				DstIP:   *service,
				DstPort: uint32(*dstPort),
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

func asPtr[T any](t T) *T {
	return &t
}
