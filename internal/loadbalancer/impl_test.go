package loadbalancer

import (
	context "context"
	"fmt"
	"testing"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

var k = koanf.New(".")

func TestCreateLB(t *testing.T) {
	config := file.Provider("../../lbmanager.yaml")
	if err := k.Load(config, yaml.Parser()); err != nil {
		t.Error(err)
	}

	lbServer, err := NewServer(k)
	if err != nil {
		t.Error(err)
	}
	defer lbServer.Close()

	lbi, err := lbServer.Create(context.TODO(), &CreateLoadBalancer{
		Name:   "test1",
		IpAddr: asPtr("10.0.0.1"),
	})
	if err != nil {
		t.Error(err)
	}
	fmt.Printf("%+v\n", lbi)
}
