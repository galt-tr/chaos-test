package topology

import "testing"

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
