package test

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestDeepE(t *testing.T) {
	cfg1 := Config{
		Name:        "test",
		LowerDevice: "eth0",
		Mode:        "bridge",
		Capacity:    0,
	}
	cfg2 := Config{
		Name:        "test",
		LowerDevice: "eth0",
		Mode:        "bridg",
		Capacity:    0,
	}
	fmt.Println(reflect.DeepEqual(cfg1, cfg2))
}

func TestMap(t *testing.T) {
	maps := make(map[string]string)
	maps["aaaaa"] = "aaaaa"
	maps["bbbbb"] = "bbbbb"
	maps["ccccc"] = "ccccc"
	for key, _ := range maps {
		if key == "bbbbb" {
			delete(maps, key)
		}
	}
	for s, s2 := range maps {
		fmt.Println(s, s2)
	}
	devices := []string{
		"eth0Mvp5",
		"eth0Mvp1",
		"eth0Mvp2",
		"eth0Mvp3",
		"eth0Mvp4",
	}
	sort.Strings(devices)
	fmt.Println(devices)
	fmt.Println(devices[0:3])
	config := &macvtapConfig{
		Config: Config{
			Name:        "test",
			LowerDevice: "eth0",
			Mode:        "bridge",
			Capacity:    0,
		},
		update: make(chan struct{}),
	}

	macvtaps := map[string]*macvtapConfig{
		"test": config,
	}

	plugin := macvtapDevicePlugin{
		macvtapConfig: config,
		NetNsPath:     "test",
		stopWatcher:   make(chan struct{}),
	}
	group := sync.WaitGroup{}
	go func() {

		for {
			time.Sleep(2 * time.Second)
			cfg := macvtaps["test"]
			cfg.Lock()
			capacity := cfg.Capacity
			cfg.Config = Config{
				Name:        "test",
				LowerDevice: "eth0",
				Mode:        "bridge",
				Capacity:    capacity + 1,
			}
			cfg.Unlock()
		}
	}()

	group.Add(1)
	go func() {

		for {
			time.Sleep(2 * time.Second)
			plugin.RLock()
			fmt.Println("current :", plugin.Capacity)
			plugin.RUnlock()
		}
	}()
	group.Add(1)

	close(plugin.update)

	select {
	case <-plugin.update:
		fmt.Println("channel update is closed")
	}

	group.Wait()

}

type Config struct {
	Name        string `json:"name"`
	LowerDevice string `json:"lowerDevice"`
	Mode        string `json:"mode"`
	Capacity    int    `json:"capacity"`
}

type macvtapConfig struct {
	sync.RWMutex
	Config
	update chan struct{}
}

type macvtapDevicePlugin struct {
	*macvtapConfig
	preferredAllocation bool
	// NetNsPath is the path to the network namespace the plugin operates in.
	NetNsPath   string
	stopWatcher chan struct{}
}
