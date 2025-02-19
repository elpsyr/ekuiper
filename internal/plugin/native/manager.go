// Copyright 2021 EMQ Technologies Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Manage the loading of both native and portable plugins

package native

import (
	"archive/zip"
	"bytes"
	"fmt"
	"github.com/lf-edge/ekuiper/internal/conf"
	"github.com/lf-edge/ekuiper/internal/meta"
	"github.com/lf-edge/ekuiper/internal/pkg/filex"
	"github.com/lf-edge/ekuiper/internal/pkg/httpx"
	"github.com/lf-edge/ekuiper/internal/pkg/store"
	plugin2 "github.com/lf-edge/ekuiper/internal/plugin"
	"github.com/lf-edge/ekuiper/pkg/api"
	"github.com/lf-edge/ekuiper/pkg/errorx"
	"github.com/lf-edge/ekuiper/pkg/kv"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"plugin"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Manager Initialized in the binder
var manager *Manager

const DELETED = "$deleted"

//Manager is append only because plugin cannot delete or reload. To delete a plugin, restart the server to reindex
type Manager struct {
	sync.RWMutex
	// 3 maps for source/sink/function. In each map, key is the plugin name, value is the version
	plugins []map[string]string
	// A map from function name to its plugin file name. It is constructed during initialization by reading kv info. All functions must have at least an entry, even the function resizes in a one function plugin.
	symbols map[string]string
	// loaded symbols in current runtime
	runtime map[string]plugin.Symbol
	// dirs
	pluginDir string
	etcDir    string
	// the access to db
	db kv.KeyValue
}

// InitManager must only be called once
func InitManager() (*Manager, error) {
	pluginDir, err := conf.GetPluginsLoc()
	if err != nil {
		return nil, fmt.Errorf("cannot find plugins folder: %s", err)
	}
	etcDir, err := conf.GetConfLoc()
	if err != nil {
		return nil, fmt.Errorf("cannot find etc folder: %s", err)
	}
	err, db := store.GetKV("pluginFuncs")
	if err != nil {
		return nil, fmt.Errorf("error when opening db: %v", err)
	}
	registry := &Manager{symbols: make(map[string]string), db: db, pluginDir: pluginDir, etcDir: etcDir, runtime: make(map[string]plugin.Symbol)}
	manager = registry
	plugins := make([]map[string]string, 3)
	for i := range plugin2.PluginTypes {
		names, err := findAll(plugin2.PluginType(i), pluginDir)
		if err != nil {
			return nil, fmt.Errorf("fail to find existing plugins: %s", err)
		}
		plugins[i] = names
	}
	registry.plugins = plugins

	for pf := range plugins[plugin2.FUNCTION] {
		l := make([]string, 0)
		if ok, err := db.Get(pf, &l); ok {
			registry.storeSymbols(pf, l)
		} else if err != nil {
			return nil, fmt.Errorf("error when querying kv: %s", err)
		} else {
			registry.storeSymbols(pf, []string{pf})
		}
	}
	return registry, nil
}

func findAll(t plugin2.PluginType, pluginDir string) (result map[string]string, err error) {
	result = make(map[string]string)
	dir := path.Join(pluginDir, plugin2.PluginTypes[t])
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}

	for _, file := range files {
		baseName := filepath.Base(file.Name())
		if strings.HasSuffix(baseName, ".so") {
			n, v := parseName(baseName)
			//load the plugins when ekuiper set up
			if !conf.IsTesting {
				if _, err := manager.loadRuntime(t, n, path.Join(dir, baseName)); err != nil {
					continue
				}
			}
			result[n] = v
		}
	}
	return
}

func GetManager() *Manager {
	return manager
}

func (rr *Manager) get(t plugin2.PluginType, name string) (string, bool) {
	rr.RLock()
	result := rr.plugins[t]
	rr.RUnlock()
	r, ok := result[name]
	return r, ok
}

func (rr *Manager) store(t plugin2.PluginType, name string, version string) {
	rr.Lock()
	rr.plugins[t][name] = version
	rr.Unlock()
}

func (rr *Manager) storeSymbols(name string, symbols []string) error {
	rr.Lock()
	defer rr.Unlock()
	for _, s := range symbols {
		if _, ok := rr.symbols[s]; ok {
			return fmt.Errorf("function name %s already exists", s)
		} else {
			rr.symbols[s] = name
		}
	}

	return nil
}

func (rr *Manager) removeSymbols(symbols []string) {
	rr.Lock()
	for _, s := range symbols {
		delete(rr.symbols, s)
	}
	rr.Unlock()
}

// API for management

func (rr *Manager) List(t plugin2.PluginType) []string {
	rr.RLock()
	result := rr.plugins[t]
	rr.RUnlock()
	keys := make([]string, 0, len(result))
	for k := range result {
		keys = append(keys, k)
	}
	return keys
}

func (rr *Manager) ListSymbols() []string {
	rr.RLock()
	result := rr.symbols
	rr.RUnlock()
	keys := make([]string, 0, len(result))
	for k := range result {
		keys = append(keys, k)
	}
	return keys
}

func (rr *Manager) GetPluginVersionBySymbol(t plugin2.PluginType, symbolName string) (string, bool) {
	switch t {
	case plugin2.FUNCTION:
		rr.RLock()
		result := rr.plugins[t]
		name, ok := rr.symbols[symbolName]
		rr.RUnlock()
		if ok {
			r, nok := result[name]
			return r, nok
		} else {
			return "", false
		}
	default:
		return rr.get(t, symbolName)
	}
}

func (rr *Manager) GetPluginBySymbol(t plugin2.PluginType, symbolName string) (string, bool) {
	switch t {
	case plugin2.FUNCTION:
		rr.RLock()
		defer rr.RUnlock()
		name, ok := rr.symbols[symbolName]
		return name, ok
	default:
		return symbolName, true
	}
}

func (rr *Manager) Register(t plugin2.PluginType, j plugin2.Plugin) error {
	name, uri, shellParas := j.GetName(), j.GetFile(), j.GetShellParas()
	//Validation
	name = strings.Trim(name, " ")
	if name == "" {
		return fmt.Errorf("invalid name %s: should not be empty", name)
	}
	if !httpx.IsValidUrl(uri) || !strings.HasSuffix(uri, ".zip") {
		return fmt.Errorf("invalid uri %s", uri)
	}

	if v, ok := rr.get(t, name); ok {
		if v == DELETED {
			return fmt.Errorf("invalid name %s: the plugin is marked as deleted but Kuiper is not restarted for the change to take effect yet", name)
		} else {
			return fmt.Errorf("invalid name %s: duplicate", name)
		}
	}

	var err error
	zipPath := path.Join(rr.pluginDir, name+".zip")

	//clean up: delete zip file and unzip files in error
	defer os.Remove(zipPath)
	//download
	err = httpx.DownloadFile(zipPath, uri)
	if err != nil {
		return fmt.Errorf("fail to download file %s: %s", uri, err)
	}

	if t == plugin2.FUNCTION {
		if len(j.GetSymbols()) > 0 {
			err = rr.db.Set(name, j.GetSymbols())
			if err != nil {
				return err
			}
			err = rr.storeSymbols(name, j.GetSymbols())
		} else {
			err = rr.storeSymbols(name, []string{name})
		}
	}
	if err != nil {
		return err
	}

	//unzip and copy to destination
	version, err := rr.install(t, name, zipPath, shellParas)
	if err == nil && len(j.GetSymbols()) > 0 {
		err = rr.db.Set(name, j.GetSymbols())
	}
	if err != nil { //Revert for any errors
		if len(j.GetSymbols()) > 0 {
			rr.removeSymbols(j.GetSymbols())
		} else {
			rr.removeSymbols([]string{name})
		}
		return fmt.Errorf("fail to install plugin: %s", err)
	}
	rr.store(t, name, version)

	switch t {
	case plugin2.SINK:
		if err := meta.ReadSinkMetaFile(path.Join(rr.etcDir, plugin2.PluginTypes[t], name+`.json`), true); nil != err {
			conf.Log.Errorf("readSinkFile:%v", err)
		}
	case plugin2.SOURCE:
		if err := meta.ReadSourceMetaFile(path.Join(rr.etcDir, plugin2.PluginTypes[t], name+`.json`), true); nil != err {
			conf.Log.Errorf("readSourceFile:%v", err)
		}
	case plugin2.FUNCTION:
		if err := meta.ReadFuncMetaFile(path.Join(rr.etcDir, plugin2.PluginTypes[t], name+`.json`), true); nil != err {
			conf.Log.Errorf("readFuncFile:%v", err)
		}
	}
	return nil
}

// RegisterFuncs prerequisite：function plugin of name exists
func (rr *Manager) RegisterFuncs(name string, functions []string) error {
	if len(functions) == 0 {
		return fmt.Errorf("property 'functions' must not be empty")
	}
	old := make([]string, 0)
	if ok, err := rr.db.Get(name, &old); err != nil {
		return err
	} else if ok {
		rr.removeSymbols(old)
	} else if !ok {
		rr.removeSymbols([]string{name})
	}
	err := rr.db.Set(name, functions)
	if err != nil {
		return err
	}
	return rr.storeSymbols(name, functions)
}

func (rr *Manager) Delete(t plugin2.PluginType, name string, stop bool) error {
	name = strings.Trim(name, " ")
	if name == "" {
		return fmt.Errorf("invalid name %s: should not be empty", name)
	}
	soPath, err := rr.getSoFilePath(t, name, true)
	if err != nil {
		return err
	}
	var results []string
	paths := []string{
		soPath,
	}
	// Find etc folder
	etcPath := path.Join(rr.etcDir, plugin2.PluginTypes[t], name)
	if fi, err := os.Stat(etcPath); err == nil {
		if fi.Mode().IsDir() {
			paths = append(paths, etcPath)
		}
	}
	switch t {
	case plugin2.SOURCE:
		paths = append(paths, path.Join(rr.etcDir, plugin2.PluginTypes[t], name+".yaml"))
		meta.UninstallSource(name)
	case plugin2.SINK:
		meta.UninstallSink(name)
	case plugin2.FUNCTION:
		old := make([]string, 0)
		if ok, err := rr.db.Get(name, &old); err != nil {
			return err
		} else if ok {
			rr.removeSymbols(old)
			err := rr.db.Delete(name)
			if err != nil {
				return err
			}
		} else if !ok {
			rr.removeSymbols([]string{name})
		}
		meta.UninstallFunc(name)
	}

	for _, p := range paths {
		_, err := os.Stat(p)
		if err == nil {
			err = os.RemoveAll(p)
			if err != nil {
				results = append(results, err.Error())
			}
		} else {
			results = append(results, fmt.Sprintf("can't find %s", p))
		}
	}

	if len(results) > 0 {
		return fmt.Errorf(strings.Join(results, "\n"))
	} else {
		rr.store(t, name, DELETED)
		if stop {
			go func() {
				time.Sleep(1 * time.Second)
				os.Exit(100)
			}()
		}
		return nil
	}
}
func (rr *Manager) GetPluginInfo(t plugin2.PluginType, name string) (map[string]interface{}, bool) {
	v, ok := rr.get(t, name)
	if strings.HasPrefix(v, "v") {
		v = v[1:]
	}
	if ok {
		r := map[string]interface{}{
			"name":    name,
			"version": v,
		}
		if t == plugin2.FUNCTION {
			l := make([]string, 0)
			if ok, _ := rr.db.Get(name, &l); ok {
				r["functions"] = l
			}
			// ignore the error
		}
		return r, ok
	}
	return nil, false
}

func (rr *Manager) install(t plugin2.PluginType, name, src string, shellParas []string) (string, error) {
	var filenames []string
	var tempPath = path.Join(rr.pluginDir, "temp", plugin2.PluginTypes[t], name)
	defer os.RemoveAll(tempPath)
	r, err := zip.OpenReader(src)
	if err != nil {
		return "", err
	}
	defer r.Close()

	haveInstallFile := false
	for _, file := range r.File {
		fileName := file.Name
		if fileName == "install.sh" {
			haveInstallFile = true
		}
	}
	if len(shellParas) != 0 && !haveInstallFile {
		return "", fmt.Errorf("have shell parameters : %s but no install.sh file", shellParas)
	}

	soPrefix := regexp.MustCompile(fmt.Sprintf(`^((%s)|(%s))(@.*)?\.so$`, name, ucFirst(name)))
	var soPath string
	var yamlFile, yamlPath, version, soName string
	expFiles := 1
	if t == plugin2.SOURCE {
		yamlFile = name + ".yaml"
		yamlPath = path.Join(rr.etcDir, plugin2.PluginTypes[t], yamlFile)
		expFiles = 2
	}
	var revokeFiles []string
	defer func() {
		if err != nil {
			for _, f := range revokeFiles {
				os.RemoveAll(f)
			}
		}
	}()
	for _, file := range r.File {
		fileName := file.Name
		if yamlFile == fileName {
			err = filex.UnzipTo(file, yamlPath)
			if err != nil {
				return version, err
			}
			revokeFiles = append(revokeFiles, yamlPath)
			filenames = append(filenames, yamlPath)
		} else if fileName == name+".json" {
			jsonPath := path.Join(rr.etcDir, plugin2.PluginTypes[t], fileName)
			if err := filex.UnzipTo(file, jsonPath); nil != err {
				conf.Log.Errorf("Failed to decompress the metadata %s file", fileName)
			} else {
				revokeFiles = append(revokeFiles, jsonPath)
			}
		} else if soPrefix.Match([]byte(fileName)) {
			soPath = path.Join(rr.pluginDir, plugin2.PluginTypes[t], fileName)
			err = filex.UnzipTo(file, soPath)
			if err != nil {
				return version, err
			}
			filenames = append(filenames, soPath)
			revokeFiles = append(revokeFiles, soPath)
			soName, version = parseName(fileName)
		} else if strings.HasPrefix(fileName, "etc/") {
			err = filex.UnzipTo(file, path.Join(rr.etcDir, plugin2.PluginTypes[t], strings.Replace(fileName, "etc", name, 1)))
			if err != nil {
				return version, err
			}
		} else { //unzip other files
			err = filex.UnzipTo(file, path.Join(tempPath, fileName))
			if err != nil {
				return version, err
			}
		}
	}
	if len(filenames) != expFiles {
		err = fmt.Errorf("invalid zip file: so file or conf file is missing")
		return version, err
	} else if haveInstallFile {
		//run install script if there is
		spath := path.Join(tempPath, "install.sh")
		shellParas = append(shellParas, spath)
		if 1 != len(shellParas) {
			copy(shellParas[1:], shellParas[0:])
			shellParas[0] = spath
		}
		cmd := exec.Command("/bin/sh", shellParas...)
		var outb, errb bytes.Buffer
		cmd.Stdout = &outb
		cmd.Stderr = &errb
		err := cmd.Run()

		if err != nil {
			conf.Log.Infof(`err:%v stdout:%s stderr:%s`, err, outb.String(), errb.String())
			return version, err
		}
		conf.Log.Infof(`run install script:%s`, outb.String())
	}

	if !conf.IsTesting {
		// load the runtime first
		_, err = manager.loadRuntime(t, soName, soPath)
		if err != nil {
			return version, err
		}
	}

	conf.Log.Infof("install %s plugin %s", plugin2.PluginTypes[t], name)
	return version, nil
}

// binder factory implementations

func (rr *Manager) Source(name string) (api.Source, error) {
	nf, err := rr.loadRuntime(plugin2.SOURCE, name, "")
	if err != nil {
		return nil, err
	}
	if nf == nil {
		return nil, nil
	}
	switch t := nf.(type) {
	case api.Source:
		return t, nil
	case func() api.Source:
		return t(), nil
	default:
		return nil, fmt.Errorf("exported symbol %s is not type of api.Source or function that return api.Source", t)
	}
}

func (rr *Manager) Sink(name string) (api.Sink, error) {
	nf, err := rr.loadRuntime(plugin2.SINK, name, "")
	if err != nil {
		return nil, err
	}
	if nf == nil {
		return nil, nil
	}
	var s api.Sink
	switch t := nf.(type) {
	case api.Sink:
		s = t
	case func() api.Sink:
		s = t()
	default:
		return nil, fmt.Errorf("exported symbol %s is not type of api.Sink or function that return api.Sink", t)
	}
	return s, nil
}

func (rr *Manager) Function(name string) (api.Function, error) {
	nf, err := rr.loadRuntime(plugin2.FUNCTION, name, "")
	if err != nil {
		return nil, err
	}
	if nf == nil {
		return nil, nil
	}
	var s api.Function
	switch t := nf.(type) {
	case api.Function:
		s = t
	case func() api.Function:
		s = t()
	default:
		return nil, fmt.Errorf("exported symbol %s is not type of api.Function or function that return api.Function", t)
	}
	return s, nil
}

func (rr *Manager) HasFunctionSet(name string) bool {
	_, ok := rr.get(plugin2.FUNCTION, name)
	return ok
}

func (rr *Manager) ConvName(name string) (string, bool) {
	_, err := rr.Function(name)
	if err == nil {
		return name, true
	}
	return name, false
}

// If not found, return nil,nil; Other errors return nil, err
func (rr *Manager) loadRuntime(t plugin2.PluginType, name, soFilepath string) (plugin.Symbol, error) {
	ut := ucFirst(name)
	ptype := plugin2.PluginTypes[t]
	key := ptype + "/" + name
	var nf plugin.Symbol
	rr.RLock()
	nf, ok := rr.runtime[key]
	rr.RUnlock()
	if !ok {
		var soPath string
		if soFilepath != "" {
			soPath = soFilepath
		} else {
			mod, err := rr.getSoFilePath(t, name, false)
			if err != nil {
				conf.Log.Warnf(fmt.Sprintf("cannot find the native plugin %s in path: %v", name, err))
				return nil, nil
			}
			soPath = mod
		}
		conf.Log.Debugf("Opening plugin %s", soPath)
		plug, err := plugin.Open(soPath)
		if err != nil {
			conf.Log.Errorf(fmt.Sprintf("plugin %s open error: %v", name, err))
			return nil, fmt.Errorf("cannot open %s: %v", soPath, err)
		}
		conf.Log.Debugf("Successfully open plugin %s", soPath)
		nf, err = plug.Lookup(ut)
		if err != nil {
			conf.Log.Warnf(fmt.Sprintf("cannot find symbol %s, please check if it is exported: %v", ut, err))
			return nil, nil
		}
		conf.Log.Debugf("Successfully look-up plugin %s", soPath)
		rr.Lock()
		rr.runtime[key] = nf
		rr.Unlock()
	}
	return nf, nil
}

// Return the lowercase version of so name. It may be upper case in path.
func (rr *Manager) getSoFilePath(t plugin2.PluginType, name string, isSoName bool) (string, error) {
	var (
		v      string
		soname string
		ok     bool
	)
	// We must identify plugin or symbol when deleting function plugin
	if isSoName {
		soname = name
	} else {
		soname, ok = rr.GetPluginBySymbol(t, name)
		if !ok {
			return "", errorx.NewWithCode(errorx.NOT_FOUND, fmt.Sprintf("invalid symbol name %s: not exist", name))
		}
	}
	v, ok = rr.get(t, soname)
	if !ok {
		return "", errorx.NewWithCode(errorx.NOT_FOUND, fmt.Sprintf("invalid name %s: not exist", soname))
	}

	soFile := soname + ".so"
	if v != "" {
		soFile = fmt.Sprintf("%s@%s.so", soname, v)
	}
	p := path.Join(rr.pluginDir, plugin2.PluginTypes[t], soFile)
	if _, err := os.Stat(p); err != nil {
		p = path.Join(rr.pluginDir, plugin2.PluginTypes[t], ucFirst(soFile))
	}
	if _, err := os.Stat(p); err != nil {
		return "", errorx.NewWithCode(errorx.NOT_FOUND, fmt.Sprintf("cannot find .so file for plugin %s", soname))
	}
	return p, nil
}

func parseName(n string) (string, string) {
	result := strings.Split(n, ".so")
	result = strings.Split(result[0], "@")
	name := lcFirst(result[0])
	if len(result) > 1 {
		return name, result[1]
	}
	return name, ""
}

func ucFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToUpper(v)) + str[i+1:]
	}
	return ""
}

func lcFirst(str string) string {
	for i, v := range str {
		return string(unicode.ToLower(v)) + str[i+1:]
	}
	return ""
}
