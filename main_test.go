package main

import (
	"encoding/json"
	"log"
	"testing"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/mackerelio/mackerel-client-go"
)

func TestCreateMetrics(t *testing.T) {
	e := env{}
	if err := envconfig.Process("", &e); err != nil {
		log.Fatal(err)
	}
	m := &mackerelAPI{e, mackerel.NewClient(e.MackerelAPIKey)}

	players := []Player{}
	err := json.Unmarshal([]byte(`[{"steamid":"Steam_76561199027850677","crossplatformid":"EOS_0002fb4f77374fcc9135cee95f33cb06","entityid":12337,"ip":"119.10.212.233","name":"KO MATTA","online":true,"position":{"x":-2348,"y":45,"z":771},"level":83.98261,"health":183,"stamina":183,"zombiekills":5917,"playerkills":0,"playerdeaths":0,"score":5906,"totalplaytime":570897,"lastonline":"2024-04-04T22:22:28","ping":41},{"steamid":"Steam_76561199026070538","crossplatformid":"EOS_0002db13368b4abaa71e546b93a79d3f","entityid":81500,"ip":"14.9.101.64","name":"Blue Destiny","online":true,"position":{"x":-1361,"y":35,"z":-231},"level":86.86746,"health":186,"stamina":186,"zombiekills":5682,"playerkills":0,"playerdeaths":1,"score":5677,"totalplaytime":289015,"lastonline":"2024-04-04T22:22:28","ping":9}]`), &players)
	if err != nil {
		t.Fatal(err)
	}
	m.postGraphDef(makeDef(players))
	metrics := m.createMetrics(players, time.Now())
	err = m.mkr.PostHostMetricValuesByHostID(m.MackerelHostID, metrics)
	if err != nil {
		t.Fatal(err)
	}
}
