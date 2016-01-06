package metric

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/golang/glog"
	"github.com/mesos-utility/docker-metrics/g"
)

func SetGlobalSetting(client DockerClient, timeout, force time.Duration, vlanPrefix, defaultVlan string) {
	gset = Setting{timeout, force, vlanPrefix, defaultVlan, client}
}

func CreateMetric(step time.Duration, client Remote, tag string, endpoint string) Metric {
	return Metric{
		Step:     step,
		Client:   client,
		Tag:      tag,
		Endpoint: endpoint,
		Stop:     make(chan bool),
	}
}

func (self *Metric) InitMetric(cid string, pid int) (err error) {
	if self.statFile, err = os.Open(fmt.Sprintf("/proc/%d/net/dev", pid)); err != nil {
		if os.IsNotExist(err) {
			glog.Warningf("container id: %s exited.", cid)
			DeleteContainerMetricMapKey(cid)
			self.Exit()
		}
		return
	}
	var info map[string]uint64
	if info, err = self.UpdateStats(cid, pid); err == nil {
		self.Last = time.Now()
		self.SaveLast(info)
	}
	return
}

func (self *Metric) Exit() {
	defer self.statFile.Close()
	self.Stop <- true
	close(self.Stop)
}

func (self *Metric) UpdateStats(cid string, pid int) (map[string]uint64, error) {
	info := map[string]uint64{}
	statsChan := make(chan *docker.Stats)
	doneChan := make(chan bool)

	if ok, _ := g.IsExists(fmt.Sprintf("/proc/%d/net/dev", pid)); !ok {
		DeleteContainerMetricMapKey(cid)
		self.Exit()
	}

	opt := docker.StatsOptions{cid, statsChan, false, doneChan, gset.timeout * time.Second}
	go func() {
		if err := gset.client.Stats(opt); err != nil {
			glog.Warningf("Get stats failed", cid[:12], err)
		}
	}()

	var stats *docker.Stats = nil
	select {
	case stats = <-statsChan:
		if stats == nil {
			return info, errors.New("Get stats failed")
		}
	case <-time.After(gset.force * time.Second):
		doneChan <- true
		return info, errors.New("Get stats timeout")
	}

	info["cpu_user"] = stats.CPUStats.CPUUsage.UsageInUsermode
	info["cpu_system"] = stats.CPUStats.CPUUsage.UsageInKernelmode
	info["cpu_usage"] = stats.CPUStats.CPUUsage.TotalUsage
	//FIXME in container it will get all CPUStats
	info["mem_usage"] = stats.MemoryStats.Usage
	info["mem_max_usage"] = stats.MemoryStats.MaxUsage
	info["mem_rss"] = stats.MemoryStats.Stats.Rss

	if err := self.getNetStats(info); err != nil {
		return info, err
	}
	return info, nil
}

func (self *Metric) SaveLast(info map[string]uint64) {
	self.Save = map[string]uint64{}
	for k, d := range info {
		self.Save[k] = d
	}
}

func (self *Metric) CalcRate(info map[string]uint64, now time.Time) (rate map[string]float64) {
	rate = map[string]float64{}
	delta := now.Sub(self.Last)
	nano_t := float64(delta.Nanoseconds())
	second_t := delta.Seconds()
	for k, d := range info {
		switch {
		case strings.HasPrefix(k, "cpu_") && d >= self.Save[k]:
			rate[fmt.Sprintf("%s_rate", k)] = float64(d-self.Save[k]) / nano_t
		case (strings.HasPrefix(k, gset.vlanPrefix) || strings.HasPrefix(k, gset.defaultVlan)) && d >= self.Save[k]:
			rate[fmt.Sprintf("%s.rate", k)] = float64(d-self.Save[k]) / second_t
		case strings.HasPrefix(k, "mem"):
			rate[k] = float64(d)
		}
	}
	self.Last = now
	return
}

func (self *Metric) Send(rate map[string]float64) error {
	step := int64(self.Step.Seconds())
	timestamp := self.Last.Unix()
	return self.Client.Send(rate, self.Endpoint, self.Tag, timestamp, step)
}
