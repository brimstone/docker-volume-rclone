package main

import (
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/brimstone/logger"

	"github.com/docker/go-plugins-helpers/volume"
)

const socketAddress = "/run/docker/plugins/rclone.sock"

var log = logger.New()

type rcloneVolume struct {
	Backend string

	Options map[string]string

	Mountpoint  string
	connections int
	process     *os.Process
}

type rcloneDriver struct {
	sync.RWMutex

	root      string
	statePath string
	volumes   map[string]*rcloneVolume
}

func newRcloneDriver(root string) (*rcloneDriver, error) {

	d := &rcloneDriver{
		root:      filepath.Join(root, "volumes"),
		statePath: filepath.Join(root, "state", "rclone-state.json"),
		volumes:   map[string]*rcloneVolume{},
	}

	data, err := ioutil.ReadFile(d.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Debug("No state found",
				log.Field("statePath", d.statePath),
			)
		} else {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, &d.volumes); err != nil {
			os.Remove(d.statePath)
			log.Error("Failed to Unmarshal state, removing file")
		}
	}

	return d, nil
}

func (d *rcloneDriver) saveState() {
	data, err := json.Marshal(d.volumes)
	if err != nil {
		return
	}

	if err := ioutil.WriteFile(d.statePath, data, 0644); err != nil {
		log.Debug("Error saving state",
			log.Field("statePath", d.statePath),
			log.Field("err", err),
		)
	}
}

func (d *rcloneDriver) Create(r *volume.CreateRequest) error {
	d.Lock()
	defer d.Unlock()
	v := &rcloneVolume{
		Options: make(map[string]string),
	}

	opts := []string{}

	for key, val := range r.Options {
		switch key {
		case "backend":
			v.Backend = val
			opts = append([]string{val}, opts...)
		default:
			if val != "" {
				v.Options[key] = val
				opts = append(opts, key+"="+val)
			} else {
				v.Options[key] = ""
				opts = append(opts, key)
			}
		}
	}

	if v.Backend == "" {
		return errors.New("'backend' option required")
	}

	// get a stable, unique value for the mount
	sort.Strings(opts)

	v.Mountpoint = filepath.Join(d.root, fmt.Sprintf("%x", md5.Sum([]byte(strings.Join(opts, " ")))))

	d.volumes[r.Name] = v

	d.saveState()

	return nil
}

func (d *rcloneDriver) Remove(r *volume.RemoveRequest) error {
	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return errors.New("volume " + r.Name + " not found")
	}

	if v.connections != 0 {
		return errors.New("volume " + r.Name + " is currently used by a container")
	}
	if err := os.RemoveAll(v.Mountpoint); err != nil {
		return err
	}
	delete(d.volumes, r.Name)
	d.saveState()
	return nil
}

func (d *rcloneDriver) Path(r *volume.PathRequest) (*volume.PathResponse, error) {
	d.RLock()
	defer d.RUnlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.PathResponse{}, errors.New("volume " + r.Name + " not found")
	}

	return &volume.PathResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *rcloneDriver) Mount(r *volume.MountRequest) (*volume.MountResponse, error) {
	log.Debug("Request to mount",
		log.Field("name", r.Name),
	)
	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.MountResponse{}, errors.New("volume " + r.Name + " not found")
	}

	log.Debug("Mount",
		log.Field("Name", r.Name),
		log.Field("Connections", v.connections),
	)
	if v.connections == 0 {
		fi, err := os.Lstat(v.Mountpoint)
		if os.IsNotExist(err) {
			if err := os.MkdirAll(v.Mountpoint, 0755); err != nil {
				return &volume.MountResponse{}, err
			}
		} else if err != nil {
			return &volume.MountResponse{}, err
		}

		if fi != nil && !fi.IsDir() {
			return &volume.MountResponse{}, errors.New(v.Mountpoint + " already exist and it's not a directory")
		}

		if err := d.mountVolume(v); err != nil {
			return &volume.MountResponse{}, err
		}
		log.Debug("Mount successful",
			log.Field("Name", r.Name),
		)
	}
	v.connections++

	return &volume.MountResponse{Mountpoint: v.Mountpoint}, nil
}

func (d *rcloneDriver) Unmount(r *volume.UnmountRequest) error {
	log.Debug("Request to umount",
		log.Field("Name", r.Name),
	)

	d.Lock()
	defer d.Unlock()
	v, ok := d.volumes[r.Name]
	if !ok {
		return errors.New("volume " + r.Name + " not found")
	}

	v.connections--

	if v.connections <= 0 {
		err := v.process.Kill()
		if err != nil {
			return err
		}
		_, err = v.process.Wait()
		if err != nil {
			return err
		}
		v.connections = 0
	}

	return nil
}

func (d *rcloneDriver) Get(r *volume.GetRequest) (*volume.GetResponse, error) {
	d.Lock()
	defer d.Unlock()

	v, ok := d.volumes[r.Name]
	if !ok {
		return &volume.GetResponse{}, errors.New("volume " + r.Name + " not found")
	}

	return &volume.GetResponse{Volume: &volume.Volume{Name: r.Name, Mountpoint: v.Mountpoint}}, nil
}

func (d *rcloneDriver) List() (*volume.ListResponse, error) {
	d.Lock()
	defer d.Unlock()

	var vols []*volume.Volume
	for name, v := range d.volumes {
		vols = append(vols, &volume.Volume{Name: name, Mountpoint: v.Mountpoint})
	}
	return &volume.ListResponse{Volumes: vols}, nil
}

func (d *rcloneDriver) Capabilities() *volume.CapabilitiesResponse {
	return &volume.CapabilitiesResponse{Capabilities: volume.Capability{Scope: "local"}}
}

func (d *rcloneDriver) mountVolume(v *rcloneVolume) error {
	cmd := exec.Command("/rclone", "config", "create", "mnt", v.Backend)
	for k, v := range v.Options {
		if v != "" {
			cmd.Args = append(cmd.Args, k, v)
		} else {
			cmd.Args = append(cmd.Args, k)
		}
	}

	log.Debug("rclone config",
		log.Field("cmd.Args", cmd.Args),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New("rclone config command execute failed: " + err.Error() + " (" + string(output) + ")")
	}
	log.Debug("configure",
		log.Field("output", output),
	)
	cmd = exec.Command("/rclone", "mount", "mnt:", v.Mountpoint)
	log.Println(cmd.Args)
	err = cmd.Start()
	if err != nil {
		return errors.New("rclone mount command start failed: " + err.Error())
	}
	v.process = cmd.Process

	return nil
}

func main() {

	d, err := newRcloneDriver("/mnt")
	if err != nil {
		log.Fatal(err)
	}
	h := volume.NewHandler(d)
	log.Info("listening",
		log.Field("socket", socketAddress),
	)
	log.Fatal(h.ServeUnix(socketAddress, 0))
}
