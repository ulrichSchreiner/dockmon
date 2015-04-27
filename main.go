package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	ui "github.com/gizak/termui"
	"github.com/samalba/dockerclient"
)

var (
	dockersocket          = flag.String("docker", "unix:///var/run/docker.sock", "the socket of the docker daemon")
	allcontainers         []dockerclient.Container
	containerDetailsIndex = 0
	containerDetailsId    = ""
	statsData             = make(map[string][]*dockerclient.Stats)
	columns               = 0
	lock                  sync.Mutex
)

type DockerDrawer func(*dockerclient.DockerClient)

func dockerStats(id string, stats *dockerclient.Stats, errs chan error, data ...interface{}) {
	lock.Lock()
	defer lock.Unlock()
	dat, _ := statsData[id]
	if len(dat) > (columns - 2) {
		dat = dat[1:]
	}
	if len(dat) > 0 && dat[len(dat)-1].Read == stats.Read {
		// same stat twice, ignore
		return
	}
	dat = append(dat, stats)
	statsData[id] = dat
}

func ContainerList() (DockerDrawer, ui.GridBufferer) {
	list := ui.NewList()
	list.ItemFgColor = ui.ColorYellow
	list.Border.Label = "Containers"
	return func(dc *dockerclient.DockerClient) {
		containers, err := dc.ListContainers(false, false, "")
		if err != nil {
			containerDetailsId = ""
			dc.StopAllMonitorStats()
			//log.Printf(err.Error())
		} else {
			var conts []string
			newstats := make(map[string][]*dockerclient.Stats)
			for i, c := range containers {
				conts = append(conts, genContainerListName(i, c, 30))
				if i == containerDetailsIndex {
					containerDetailsId = c.Id
				}
				stat, ok := statsData[c.Id]
				if ok {
					newstats[c.Id] = stat
				} else {
					errs := make(chan error)
					dc.StartMonitorStats(c.Id, dockerStats, errs, &c)
				}
			}
			lock.Lock()
			defer lock.Unlock()
			statsData = newstats
			allcontainers = containers
			if len(allcontainers) == 0 {
				dc.StopAllMonitorStats()
				containerDetailsId = ""
			}
			list.Items = conts
			list.Height = len(conts) + 2
		}
	}, list
}

func genContainerListName(idx int, c dockerclient.Container, maxlen int) string {
	s := fmt.Sprintf("[%d] %s:%s", idx, c.Names[0], c.Id)
	if len(s) > maxlen {
		return s[:maxlen-3] + "..."
	}
	return s
}

func ContainerDetails() (DockerDrawer, ui.GridBufferer) {
	list := ui.NewList()
	list.ItemFgColor = ui.ColorYellow
	list.Border.Label = "Details"
	return func(dc *dockerclient.DockerClient) {
		if containerDetailsId == "" {
			list.Height = 2
			return
		}
		ci, err := dc.InspectContainer(containerDetailsId)
		if err != nil {
			//log.Printf(err.Error())
		} else {
			var lines []string
			lines = append(lines, fmt.Sprintf("Name: %s", ci.Name))
			lines = append(lines, fmt.Sprintf("Image: %s", ci.Image))
			lines = append(lines, fmt.Sprintf("Path: %s", ci.Path))
			lines = append(lines, fmt.Sprintf("Args: %s", ci.Args))
			lines = append(lines, fmt.Sprintf("IP: %s", ci.NetworkSettings.IPAddress))
			lines = append(lines, fmt.Sprintf("Ports: %s", genPortMappings(ci)))
			for vi, v := range genVolumes(ci) {
				if vi == 0 {
					lines = append(lines, fmt.Sprintf("Volumes: %s", v))
				} else {
					lines = append(lines, fmt.Sprintf("         %s", v))
				}
			}
			lines = append(lines, fmt.Sprintf("Hostname: %s", ci.Config.Hostname))
			lines = append(lines, fmt.Sprintf("Memory: %d", ci.Config.Memory))
			lines = append(lines, fmt.Sprintf("Swap: %d", ci.Config.MemorySwap))
			lines = append(lines, fmt.Sprintf("Cpu-Shares: %d", ci.Config.CpuShares))
			lines = append(lines, fmt.Sprintf("Cpu-Set: %s", ci.Config.Cpuset))
			lines = append(lines, fmt.Sprintf("Env: %s", ci.Config.Env))
			list.Items = lines
			list.Height = len(lines) + 2
			list.Border.Label = fmt.Sprintf("Details: %s", ci.Name)
		}
	}, list
}

func genPortMappings(di *dockerclient.ContainerInfo) string {
	var res []string
	var keys []string
	for p, _ := range di.NetworkSettings.Ports {
		keys = append(keys, p)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pc := di.NetworkSettings.Ports[k]
		res = append(res, fmt.Sprintf("%s -> %s ", k, pc))
	}

	return strings.Join(res, ",")
}

func genVolumes(di *dockerclient.ContainerInfo) []string {
	var res []string
	var keys []string
	for v, _ := range di.Volumes {
		keys = append(keys, v)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := di.Volumes[k]
		res = append(res, fmt.Sprintf("%s -> %s ", k, v))
	}
	return res
}

func ContainerCpu() (DockerDrawer, ui.GridBufferer) {
	cpus := ui.NewSparklines()
	cpus.Border.Label = "CPU"
	return func(dc *dockerclient.DockerClient) {
		cpus.Lines = []ui.Sparkline{}
		cpus.Height = 2
		for _, c := range allcontainers {
			dat, _ := statsData[c.Id]
			lastVal := 0
			if len(dat) > 1 {
				lastVal = cpuPercent(dat, len(dat)-1)
			}
			l := ui.NewSparkline()
			l.Title = fmt.Sprintf("[%d %%] %s:%s ", lastVal, c.Names, c.Id)
			l.LineColor = ui.ColorRed
			l.Data = genCPUSystemUsage(dat)
			cpus.Lines = append(cpus.Lines, l)
			cpus.Height = cpus.Height + 2
		}

	}, cpus
}

func ContainerMemory() (DockerDrawer, ui.GridBufferer) {
	mem := ui.NewMBarChart()
	mem.Border.Label = "Memory usage (red=used, green=free)"
	mem.Height = 20
	mem.BarWidth = 5
	mem.BarColor[0] = ui.ColorRed
	mem.BarColor[1] = ui.ColorGreen
	return func(dc *dockerclient.DockerClient) {
		var labels []string
		var used []int
		var free []int
		for i, c := range allcontainers {
			labels = append(labels, fmt.Sprintf("[%d]", i))
			dat, _ := statsData[c.Id]
			if len(dat) > 1 {
				last := dat[len(dat)-1]
				memused := last.MemoryStats.Usage
				memlim := last.MemoryStats.Limit
				memusedP := int(100 * memused / memlim)
				used = append(used, memusedP)
				free = append(free, 100-memusedP)
			}
		}
		mem.DataLabels = labels
		mem.Data[0] = used
		mem.Data[1] = free
	}, mem
}

func genCPUSystemUsage(stats []*dockerclient.Stats) []int {
	var res []int
	for i, _ := range stats {
		if i > 0 {
			res = append(res, cpuPercent(stats, i))
		}
	}
	return res
}

func cpuPercent(stats []*dockerclient.Stats, idx int) int {
	var (
		p        = 0.0
		mystat   = stats[idx]
		prevstat = stats[idx-1]
		cpudelta = float64(mystat.CpuStats.CpuUsage.TotalUsage - prevstat.CpuStats.CpuUsage.TotalUsage)
		sysdelta = float64(mystat.CpuStats.SystemUsage - prevstat.CpuStats.SystemUsage)
	)

	if sysdelta > 0.0 && cpudelta > 0.0 {
		p = (cpudelta / sysdelta) * float64(len(mystat.CpuStats.CpuUsage.PercpuUsage)) * 100.0
	}
	return int(p)
}

func main() {
	flag.Parse()

	err := ui.Init()
	if err != nil {
		panic(err)
	}
	defer ui.Close()

	// Init the client
	docker, err := dockerclient.NewDockerClient(*dockersocket, nil)
	if err != nil {
		panic(err)
	}

	var drawers []DockerDrawer
	containerlist, uiCntList := ContainerList()
	containerDetails, uiCntDets := ContainerDetails()
	cpuList, uiCpus := ContainerCpu()
	memUsg, uiMem := ContainerMemory()

	drawers = append(drawers, containerlist, containerDetails, cpuList, memUsg)

	title := ui.NewPar("dockmon (press 'q' to quit)")
	title.Height = 3
	title.HasBorder = true

	ui.Body.AddRows(
		ui.NewRow(
			ui.NewCol(12, 0, title)),
		ui.NewRow(
			ui.NewCol(4, 0, uiCntList),
			ui.NewCol(8, 0, uiCntDets)),
		ui.NewRow(
			ui.NewCol(12, 0, uiCpus)),
		ui.NewRow(
			ui.NewCol(6, 0, uiMem)))

	// calculate layout
	ui.Body.Align()
	columns = ui.Body.Width

	evt := ui.EventCh()

	for {
		select {
		case e := <-evt:
			if e.Type == ui.EventKey && e.Ch == 'q' {
				return
			}
			if e.Type == ui.EventKey && e.Ch >= '0' && e.Ch <= '9' {
				containerDetailsIndex = int(e.Ch - '0')
			}
			if e.Type == ui.EventResize {
				ui.Body.Width = ui.TermWidth()
				ui.Body.Align()
			}
		default:
			for _, d := range drawers {
				d(docker)
			}
			ui.Body.Align()
			columns = ui.Body.Width
			ui.Render(ui.Body)
			time.Sleep(time.Second / 2)
		}
	}
}
