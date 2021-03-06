package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"sync"

	ui "github.com/gizak/termui"
	"github.com/samalba/dockerclient"
)

const (
	kb      = 1024
	mb      = kb * 1024
	gb      = mb * 1024
	tb      = gb * 1024
	version = "0.1"
)

var (
	dockersocket          = flag.String("docker", "unix:///var/run/docker.sock", "the socket of the docker daemon")
	allcontainers         []dockerclient.Container
	containerDetailsIndex = 0
	containerDetailsID    = ""
	statsData             = make(map[string][]*dockerclient.Stats)
	lock                  sync.Mutex
	uiStack               []*ui.Grid
)

type dockerDrawer func(*dockerclient.DockerClient)

type networkDiffer func(cur *dockerclient.NetworkStats, prev *dockerclient.NetworkStats) int

func dockerStats(id string, stats *dockerclient.Stats, errs chan error, data ...interface{}) {
	lock.Lock()
	defer lock.Unlock()
	dat, _ := statsData[id]
	// if we have more stats than visible columns in console, scroll.
	if len(dat) > (ui.Body.Width - 2) {
		dat = dat[1:]
	}
	if len(dat) > 0 && dat[len(dat)-1].Read == stats.Read {
		// same stat twice, ignore
		return
	}
	dat = append(dat, stats)
	statsData[id] = dat
}

func containerList() (dockerDrawer, ui.GridBufferer) {
	list := ui.NewList()
	list.ItemFgColor = ui.ColorYellow
	list.BorderLabel = "Containers (#num for details)"
	return func(dc *dockerclient.DockerClient) {
		containers, err := dc.ListContainers(false, false, "")
		if err != nil {
			containerDetailsID = ""
			dc.StopAllMonitorStats()
		} else {
			var conts []string
			newstats := make(map[string][]*dockerclient.Stats)
			for i, c := range containers {
				conts = append(conts, genContainerListName(i, c, 30))
				if i == containerDetailsIndex {
					containerDetailsID = c.Id
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
				containerDetailsID = ""
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

func containerDetails() (dockerDrawer, ui.GridBufferer) {
	list := ui.NewList()
	list.ItemFgColor = ui.ColorYellow
	list.BorderLabel = "Details"
	return func(dc *dockerclient.DockerClient) {
		if containerDetailsID == "" {
			list.Height = 2
			return
		}
		ci, err := dc.InspectContainer(containerDetailsID)
		if err != nil {
			// don't log !
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
			list.BorderLabel = fmt.Sprintf("Details: %s", ci.Name)
		}
	}, list
}

func genPortMappings(di *dockerclient.ContainerInfo) string {
	var res []string
	var keys []string
	for p := range di.NetworkSettings.Ports {
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
	for v := range di.Volumes {
		keys = append(keys, v)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := di.Volumes[k]
		res = append(res, fmt.Sprintf("%s -> %s ", k, v))
	}
	return res
}

func containerCPU() (dockerDrawer, ui.GridBufferer) {
	cpus := ui.NewSparklines()
	cpus.BorderLabel = "CPU"
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
			l.LineColor = ui.ColorYellow
			l.Data = genCPUSystemUsage(dat)
			l.Height = 2
			cpus.Lines = append(cpus.Lines, l)
			cpus.Height = cpus.Height + 3
		}

	}, cpus
}

func containerNetworkBytes(lbl string, differ networkDiffer, color ui.Attribute) (dockerDrawer, ui.GridBufferer) {
	netw := ui.NewSparklines()
	netw.BorderLabel = lbl
	return func(dc *dockerclient.DockerClient) {
		netw.Lines = []ui.Sparkline{}
		netw.Height = 2
		for idx, c := range allcontainers {
			dat, _ := statsData[c.Id]
			if len(dat) > 1 {
				l := ui.NewSparkline()
				l.LineColor = color
				l.Data = genNetwork(dat, differ)
				tx := 0
				if len(l.Data) > 0 {
					tx = l.Data[len(l.Data)-1]
				}
				l.Title = fmt.Sprintf("[%5s] %s", memAsString(uint64(tx)), genContainerListName(idx, c, 20))
				l.Height = 2
				netw.Lines = append(netw.Lines, l)
				netw.Height = netw.Height + 3
			}
		}

	}, netw
}

func containerPercentMemory() (dockerDrawer, ui.GridBufferer) {
	mem := ui.NewBarChart()
	mem.BorderLabel = "Memory % usage "
	mem.Height = 13
	mem.BarWidth = 5
	mem.SetMax(100)
	mem.BarColor = ui.ColorRed
	return func(dc *dockerclient.DockerClient) {
		var labels []string
		var used []int
		for i, c := range allcontainers {
			labels = append(labels, fmt.Sprintf("[%2d]", i))
			dat, _ := statsData[c.Id]
			if len(dat) > 1 {
				last := dat[len(dat)-1]
				memused := last.MemoryStats.Usage
				memlim := last.MemoryStats.Limit
				memusedP := int(100 * memused / memlim)
				used = append(used, memusedP)
			}
		}
		mem.DataLabels = labels
		mem.Data = used
		mx := findMaxInt(used)
		if mx < 30 {
			mem.SetMax(mx * 3)
		} else {
			mem.SetMax(100)
		}
	}, mem
}

func containerValueMemory() (dockerDrawer, ui.GridBufferer) {
	list := ui.NewList()
	list.ItemFgColor = ui.ColorYellow
	list.BorderLabel = "Container Memory"

	return func(dc *dockerclient.DockerClient) {
		var labels []string
		for i, c := range allcontainers {
			dat, _ := statsData[c.Id]
			var memused uint64
			if len(dat) > 1 {
				last := dat[len(dat)-1]
				memused = last.MemoryStats.Usage
			}
			labels = append(labels, fmt.Sprintf("[%2d]: %s", i, memAsString(memused)))
		}
		list.Items = labels
		list.Height = len(labels) + 2
	}, list
}

func genCPUSystemUsage(stats []*dockerclient.Stats) []int {
	var res []int
	for i := range stats {
		if i > 0 {
			res = append(res, cpuPercent(stats, i))
		}
	}
	return res
}

func genNetwork(stats []*dockerclient.Stats, differ networkDiffer) []int {
	var res []int
	for i := range stats {
		if i > 0 {
			stat1 := stats[i]
			stat2 := stats[i-1]
			res = append(res, differ(&stat1.NetworkStats, &stat2.NetworkStats))
		}
	}
	return res
}

func rxDiffer(cur *dockerclient.NetworkStats, prev *dockerclient.NetworkStats) int {
	return int(cur.RxBytes - prev.RxBytes)
}
func txDiffer(cur *dockerclient.NetworkStats, prev *dockerclient.NetworkStats) int {
	return int(cur.TxBytes - prev.TxBytes)
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

func memAsString(val uint64) string {
	if val >= tb {
		tval := int(val / tb)
		return fmt.Sprintf("%dtb", tval)
	}
	if val >= gb {
		gval := int(val / gb)
		return fmt.Sprintf("%dgb", gval)
	}
	if val >= mb {
		mval := int(val / mb)
		return fmt.Sprintf("%dmb", mval)
	}
	if val >= kb {
		kval := int(val / kb)
		return fmt.Sprintf("%dkb", kval)
	}
	return fmt.Sprintf("%db", val)
}

func findMaxInt(vals []int) int {
	if len(vals) < 1 {
		return 0
	}
	max := vals[0]
	for _, v := range vals {
		if v > max {
			max = v
		}
	}
	return max
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

	var drawers []dockerDrawer
	containerlist, uiCntList := containerList()
	containerDetails, uiCntDets := containerDetails()
	cpuList, uiCpus := containerCPU()
	memUsg, uiMem := containerPercentMemory()
	memVal, uiMemVal := containerValueMemory()
	rxVal, uiRx := containerNetworkBytes("Rx Bytes", rxDiffer, ui.ColorGreen)
	txVal, uiTx := containerNetworkBytes("Tx Bytes", txDiffer, ui.ColorBlue)

	drawers = append(drawers, containerlist, containerDetails, cpuList, memUsg, memVal, rxVal, txVal)

	title := ui.NewPar(fmt.Sprintf("dockmon %s ('q' to quit panel)", version))
	title.Height = 3
	title.Border = true

	mainGrid := mainPanel(title, uiCntList, uiCpus, uiMem, uiMemVal, uiRx, uiTx)
	detailsGrid := detailsPanel(title, uiCntDets)

	ui.Body = pushPanel(mainGrid)
	ui.Body.Width = ui.TermWidth()
	ui.Body.Align()

	//ui.Render(ui)

	ui.Handle("/sys/kbd/", func(evt ui.Event) {
		ch := evt.Data.(ui.EvtKbd)
		key := ch.KeyStr[0]
		if key == 'q' {
			_, err := popPanel()
			if err != nil {
				ui.StopLoop()
			}
		}
		if key >= '0' && key <= '9' {
			containerDetailsIndex = int(key - '0')
			pushPanel(detailsGrid)
		}
	})
	ui.Handle("/timer/1s", func(e ui.Event) {
		for _, d := range drawers {
			d(docker)
		}
		ui.Body.Align()
		ui.Render(ui.Body)
	})
	ui.Handle("/sys/wnd/resize", func(e ui.Event) {
		ui.Body.Width = ui.TermWidth()
		ui.Body.Align()
	})
	ui.Loop()
}

func pushPanel(p *ui.Grid) *ui.Grid {
	uiStack = append(uiStack, p)
	ui.Body = p
	ui.Body.Width = ui.TermWidth()
	ui.Body.Align()
	return p
}

func popPanel() (*ui.Grid, error) {
	if len(uiStack) < 2 {
		return nil, fmt.Errorf("no more panels in stack")
	}
	_, uiStack = uiStack[len(uiStack)-1], uiStack[:len(uiStack)-1]
	last := uiStack[len(uiStack)-1]
	ui.Body = last
	ui.Body.Width = ui.TermWidth()
	ui.Body.Align()
	return last, nil
}

func mainPanel(title, cntList, cpus, mem, memval, rx, tx ui.GridBufferer) *ui.Grid {
	p := &ui.Grid{}

	p.AddRows(
		ui.NewRow(
			ui.NewCol(12, 0, title)),
		ui.NewRow(
			ui.NewCol(3, 0, cntList),
			ui.NewCol(6, 0, mem),
			ui.NewCol(3, 0, memval)),
		ui.NewRow(
			ui.NewCol(6, 0, cpus),
			ui.NewCol(3, 0, rx),
			ui.NewCol(3, 0, tx)))

	return p
}

func detailsPanel(title, details ui.GridBufferer) *ui.Grid {
	p := &ui.Grid{}

	p.AddRows(
		ui.NewRow(
			ui.NewCol(12, 0, title)),
		ui.NewRow(
			ui.NewCol(12, 0, details)))

	return p
}
