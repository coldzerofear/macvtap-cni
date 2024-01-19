package deviceplugin

import (
	"encoding/json"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/fsnotify/fsnotify"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"github.com/kubevirt/macvtap-cni/pkg/util"
	"os"
	"reflect"
	"time"
)

func (ml *macvtapLister) ConfigPathDiscover(pluginListCh chan dpm.PluginNameList) {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		glog.Errorf("new file watcher failed: %v", err)
		os.Exit(1)
	}
	defer fsWatcher.Close()

	var pushPluginList = func() error {
		var plugins = make(dpm.PluginNameList, 0)
		newConfig, err := readConfigByPath(ConfigMapFilePath)
		if err != nil {
			glog.Errorf("Error reading config[Path:%s]: %v", ConfigMapFilePath, err)
			return err
		}
		glog.V(3).Infof("Read configuration %+v", newConfig)

		ml.Lock()
		for _, config := range newConfig {
			plugins = append(plugins, config.Name)
			if macvtapCfg, ok := ml.Config[config.Name]; ok {
				if !reflect.DeepEqual(macvtapCfg.Config, config) {
					macvtapCfg.Lock()
					macvtapCfg.Config = config
					macvtapCfg.Unlock()
					macvtapCfg.update <- struct{}{}
				}
			} else {
				ml.Config[config.Name] = &macvtapConfig{
					Config: config,
					update: make(chan struct{}),
				}
			}
		}
		// 删除已不存在的配置，防止内存泄漏
		for name, config := range ml.Config {
			found := false
			for _, pluginName := range plugins {
				if name == pluginName {
					found = true
					break
				}
			}
			if !found {
				close(config.update)
				delete(ml.Config, name)
			}
		}
		ml.Unlock()
		if len(plugins) > 0 {
			pluginListCh <- plugins
			return nil
		}
		return ml.discoverByLinks(pluginListCh, false)
	}
loop:
	if err = fsWatcher.Add(ConfigMapFilePath); err != nil {
		glog.Errorf("add config file [%s] watcher failed: %v", ConfigMapFilePath, err)
		os.Exit(1)
	}

	if err = pushPluginList(); err != nil {
		glog.Errorf("pushPluginList error: %v", err)
		os.Exit(1)
	}

	for {
		select {
		case event, open := <-fsWatcher.Events:
			if !open {
				glog.Error("watcher event channel closed, exit code 1")
				os.Exit(1)
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				if err = pushPluginList(); err != nil {
					glog.Errorf("pushPluginList error: %v", err)
				}
			} else if event.Op&fsnotify.Write == fsnotify.Write {
				if err = pushPluginList(); err != nil {
					glog.Errorf("pushPluginList error: %v", err)
				}
			} else if event.Op&fsnotify.Rename == fsnotify.Rename {
				glog.Errorf("%s rename, restart watcher...", event.Name)
				time.Sleep(1 * time.Second)
				goto loop
			} else if event.Op&fsnotify.Remove == fsnotify.Remove {
				// TODO 配置文件被删除，重启watcher
				glog.Errorf("%s removed, restart watcher...", event.Name)
				time.Sleep(1 * time.Second)
				goto loop
			}
		case err := <-fsWatcher.Errors:
			glog.Errorf("configmap watching error: %v", err)
		}
	}

}

func (ml *macvtapLister) ConfigEnvDiscover(pluginListCh chan dpm.PluginNameList) {

	var plugins = make(dpm.PluginNameList, 0)

	config, err := readConfigByEnv(EnvName)
	if err != nil {
		glog.Errorf("Error reading config[Env:%s]: %v", EnvName, err)
		os.Exit(1)
	}

	glog.V(3).Infof("Read configuration %+v", config)

	for _, macvtapConfig := range config {
		plugins = append(plugins, macvtapConfig.Name)
	}

	// Configuration is static and we don't need to do anything else
	if len(config) > 0 {
		ml.Lock()
		defer ml.Unlock()
		for _, cfg := range config {
			ml.Config[cfg.Name] = &macvtapConfig{
				Config: cfg,
				update: make(chan struct{}),
			}
		}
		pluginListCh <- plugins
		return
	}

	// If there was no configuration, we setup resources based on the existing
	// links of the host.
	if err = ml.discoverByLinks(pluginListCh, true); err != nil {
		os.Exit(1)
	}
}

func readConfigByPath(configPath string) (map[string]Config, error) {
	var configs []Config
	configMap := make(map[string]Config)
	jsonBytes, err := os.ReadFile(configPath)
	if err != nil {
		return configMap, err
	}

	if err := json.Unmarshal(jsonBytes, &configs); err != nil {
		return configMap, err
	}

	for _, cfg := range configs {
		configMap[cfg.Name] = cfg
	}

	return configMap, nil
}

func readConfigByEnv(envName string) (map[string]Config, error) {
	var configs []Config
	configMap := make(map[string]Config)

	configEnv := os.Getenv(envName)
	err := json.Unmarshal([]byte(configEnv), &configs)
	if err != nil {
		return configMap, err
	}

	for _, cfg := range configs {
		configMap[cfg.Name] = cfg
	}

	return configMap, nil
}

func (ml *macvtapLister) discoverByLinks(pluginListCh chan dpm.PluginNameList, keepRun bool) error {
	// To know when the manager is stoping, we need to read from pluginListCh.
	// We avoid reading our own updates by using a middle channel.
	// We buffer up to one msg because of the initial call to sendSuitableParents.
	parentListCh := make(chan []string, 1)
	defer close(parentListCh)

	sendSuitableParents := func() error {
		var linkNames []string
		err := ns.WithNetNSPath(ml.NetNsPath, func(_ ns.NetNS) error {
			var err error
			linkNames, err = util.FindSuitableMacvtapParents()
			return err
		})

		if err != nil {
			glog.Errorf("Error while finding links: %v", err)
			return err
		}

		parentListCh <- linkNames
		return nil
	}

	// Do an initial search to catch early permanent runtime problems
	err := sendSuitableParents()
	if err != nil {
		return err
	}
	if keepRun {
		// Keep updating on changes for suitable parents.
		stop := make(chan struct{})
		defer close(stop)
		go util.OnSuitableMacvtapParentEvent(
			ml.NetNsPath,
			// Wrapper to ignore error
			func() {
				sendSuitableParents()
			},
			stop,
			func(err error) {
				glog.Error(err)
			})
	}

	// Keep forwarding updates to the manager until it closes down
	for {
		select {
		case parentNames := <-parentListCh:
			ml.Lock()
			for _, name := range parentNames {
				if _, ok := ml.Config[name]; !ok {
					ml.Config[name] = &macvtapConfig{
						Config: Config{
							Name:        name,
							LowerDevice: name,
							Mode:        DefaultMode,
							Capacity:    DefaultCapacity,
						},
						update: make(chan struct{}),
					}
				}
			}
			ml.Unlock()
			pluginListCh <- parentNames
			if !keepRun {
				return nil
			}
		case _, open := <-pluginListCh:
			if !open {
				return nil
			}
		}
	}
}
