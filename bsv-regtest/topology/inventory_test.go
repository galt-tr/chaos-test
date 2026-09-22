package topology

import (
	"encoding/json"
	"testing"
)

func TestUseHostURLs(t *testing.T) {
	inv := &Inventory{
		Nodes: []Node{{Name: "n1", RPCURL: "http://10.191.0.11:9292", HostRPCURL: "http://localhost:21292", AssetURL: "http://10.191.0.11:8090/api/v1", HostAssetURL: "http://localhost:20090/api/v1"}},
		Hub:   &Hub{APIURL: "http://10.191.0.30:3000", HostAPI: "http://localhost:3000"},
		Services: map[string]Service{
			"arcade": {URL: "http://10.191.0.40:8080", HostURL: "http://localhost:18080", URLs: map[string]URLPair{
				"chaintracks": {URL: "http://10.191.0.40:8083", HostURL: "http://localhost:18083"},
				"internal":    {URL: "http://10.191.0.40:9"},
			}},
			"kafka": {URL: "kafka-shared:9092"},
		},
	}
	inv.UseHostURLs()
	if inv.Nodes[0].RPCURL != "http://localhost:21292" || inv.Nodes[0].AssetURL != "http://localhost:20090/api/v1" {
		t.Fatalf("node URLs not rewritten: %+v", inv.Nodes[0])
	}
	if inv.Hub.APIURL != "http://localhost:3000" {
		t.Fatalf("hub URL not rewritten: %s", inv.Hub.APIURL)
	}
	a := inv.Services["arcade"]
	if a.URL != "http://localhost:18080" || a.HostURL != "http://localhost:18080" {
		t.Fatalf("arcade URL: %+v", a)
	}
	if a.URLs["chaintracks"].URL != "http://localhost:18083" {
		t.Fatalf("chaintracks URL not rewritten: %+v", a.URLs["chaintracks"])
	}
	if a.URLs["internal"].URL != "http://10.191.0.40:9" {
		t.Fatalf("pair without host URL must be untouched: %+v", a.URLs["internal"])
	}
	if inv.Services["kafka"].URL != "kafka-shared:9092" {
		t.Fatalf("kafka must be untouched")
	}
}

func TestNodeLookupSkipsSVIndex(t *testing.T) {
	inv := &Inventory{Nodes: []Node{
		{Kind: "teranode", Name: "teranode1", Index: 1},
		{Kind: "teranode", Name: "teranode2", Index: 2},
		{Kind: "svnode", Name: "svnode1", Index: 1, Sidecar: &Sidecar{APIURL: "http://10.191.0.31:3000", HostAPI: "http://localhost:40300"}},
	}}
	n, err := inv.Node("1")
	if err != nil || n.Name != "teranode1" {
		t.Fatalf("index 1 must be teranode1, got %v %v", n, err)
	}
	if n, err := inv.Node("svnode1"); err != nil || !n.IsSV() {
		t.Fatalf("svnode1 by name: %v %v", n, err)
	}
	if len(inv.Teranodes()) != 2 || len(inv.SVNodes()) != 1 {
		t.Fatalf("kind filters: %d teranodes, %d sv", len(inv.Teranodes()), len(inv.SVNodes()))
	}
	inv.UseHostURLs()
	if inv.Nodes[2].Sidecar.APIURL != "http://localhost:40300" {
		t.Fatalf("sidecar URL not rewritten: %s", inv.Nodes[2].Sidecar.APIURL)
	}
}

func TestNetworkNamesRoundTrip(t *testing.T) {
	in := Inventory{Project: "bsv-regtest", Networks: Networks{Chaos: "10.190.0.0/24", ChaosName: "bsv-regtest_chaosnet", AlertName: "bsv-regtest_alertnet", CtlName: "bsv-regtest_ctlnet"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Inventory
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Project != "bsv-regtest" || out.Networks.ChaosName != "bsv-regtest_chaosnet" || out.Networks.AlertName != "bsv-regtest_alertnet" || out.Networks.CtlName != "bsv-regtest_ctlnet" || out.Networks.Chaos != "10.190.0.0/24" {
		t.Fatalf("round trip lost fields: %+v", out)
	}
	// Older inventories without names still decode; consumers must treat "" as "re-run gen".
	var legacy Inventory
	if err := json.Unmarshal([]byte(`{"networks":{"chaosnet":"10.190.0.0/24"}}`), &legacy); err != nil || legacy.Networks.ChaosName != "" {
		t.Fatalf("legacy decode: %v %+v", err, legacy.Networks)
	}
}
