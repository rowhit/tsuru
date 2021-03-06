// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"archive/tar"
	"bytes"
	stderr "errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/tsuru/config"
	"github.com/tsuru/docker-cluster/cluster"
	clusterLog "github.com/tsuru/docker-cluster/log"
	"github.com/tsuru/tsuru/action"
	"github.com/tsuru/tsuru/api/shutdown"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/cmd"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/db/storage"
	"github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/docker/container"
	"github.com/tsuru/tsuru/provision/docker/healer"
	"github.com/tsuru/tsuru/provision/docker/nodecontainer"
	"github.com/tsuru/tsuru/router"
	_ "github.com/tsuru/tsuru/router/galeb"
	_ "github.com/tsuru/tsuru/router/hipache"
	_ "github.com/tsuru/tsuru/router/routertest"
	_ "github.com/tsuru/tsuru/router/vulcand"
	"gopkg.in/mgo.v2/bson"
)

var mainDockerProvisioner *dockerProvisioner
var ErrEntrypointOrProcfileNotFound = stderr.New("You should provide a entrypoint in image or a Procfile in the following locations: /home/application/current or /app/user or /.")

func init() {
	mainDockerProvisioner = &dockerProvisioner{}
	provision.Register("docker", mainDockerProvisioner)
}

func getRouterForApp(app provision.App) (router.Router, error) {
	routerName, err := app.GetRouter()
	if err != nil {
		return nil, err
	}
	return router.Get(routerName)
}

type dockerProvisioner struct {
	cluster        *cluster.Cluster
	collectionName string
	storage        cluster.Storage
	scheduler      *segregatedScheduler
	isDryMode      bool
	nodeHealer     *healer.NodeHealer
	actionLimiter  provision.ActionLimiter
}

func (p *dockerProvisioner) initDockerCluster() error {
	debug, _ := config.GetBool("debug")
	clusterLog.SetDebug(debug)
	clusterLog.SetLogger(log.GetStdLogger())
	var err error
	if p.storage == nil {
		p.storage, err = buildClusterStorage()
		if err != nil {
			return err
		}
	}
	if p.collectionName == "" {
		var name string
		name, err = config.GetString("docker:collection")
		if err != nil {
			return err
		}
		p.collectionName = name
	}
	var nodes []cluster.Node
	TotalMemoryMetadata, _ := config.GetString("docker:scheduler:total-memory-metadata")
	maxUsedMemory, _ := config.GetFloat("docker:scheduler:max-used-memory")
	p.scheduler = &segregatedScheduler{
		maxMemoryRatio:      float32(maxUsedMemory),
		TotalMemoryMetadata: TotalMemoryMetadata,
		provisioner:         p,
	}
	p.cluster, err = cluster.New(p.scheduler, p.storage, nodes...)
	if err != nil {
		return err
	}
	p.cluster.AddHook(cluster.HookEventBeforeContainerCreate, &nodecontainer.ClusterHook{Provisioner: p})
	autoHealingNodes, _ := config.GetBool("docker:healing:heal-nodes")
	if autoHealingNodes {
		disabledSeconds, _ := config.GetInt("docker:healing:disabled-time")
		if disabledSeconds <= 0 {
			disabledSeconds = 30
		}
		maxFailures, _ := config.GetInt("docker:healing:max-failures")
		if maxFailures <= 0 {
			maxFailures = 5
		}
		waitSecondsNewMachine, _ := config.GetInt("docker:healing:wait-new-time")
		if waitSecondsNewMachine <= 0 {
			waitSecondsNewMachine = 5 * 60
		}
		p.nodeHealer = healer.NewNodeHealer(healer.NodeHealerArgs{
			Provisioner:           p,
			DisabledTime:          time.Duration(disabledSeconds) * time.Second,
			WaitTimeNewMachine:    time.Duration(waitSecondsNewMachine) * time.Second,
			FailuresBeforeHealing: maxFailures,
		})
		shutdown.Register(p.nodeHealer)
		p.cluster.Healer = p.nodeHealer
		p.cluster.AddHook(cluster.HookEventBeforeNodeUnregister, p.nodeHealer)
	}
	healContainersSeconds, _ := config.GetInt("docker:healing:heal-containers-timeout")
	if healContainersSeconds > 0 {
		contHealerInst := healer.NewContainerHealer(healer.ContainerHealerArgs{
			Provisioner:         p,
			MaxUnresponsiveTime: time.Duration(healContainersSeconds) * time.Second,
			Done:                make(chan bool),
			Locker:              &appLocker{},
		})
		shutdown.Register(contHealerInst)
		go contHealerInst.RunContainerHealer()
	}
	activeMonitoring, _ := config.GetInt("docker:healing:active-monitoring-interval")
	if activeMonitoring > 0 {
		p.cluster.StartActiveMonitoring(time.Duration(activeMonitoring) * time.Second)
	}
	autoScale := p.initAutoScaleConfig()
	if autoScale.Enabled {
		shutdown.Register(autoScale)
		go autoScale.run()
	}
	limitMode, _ := config.GetString("docker:limit:mode")
	if limitMode == "global" {
		p.actionLimiter = &provision.MongodbLimiter{}
	} else {
		p.actionLimiter = &provision.LocalLimiter{}
	}
	actionLimit, _ := config.GetUint("docker:limit:actions-per-host")
	if actionLimit > 0 {
		p.actionLimiter.Initialize(actionLimit)
	}
	return nil
}

func (p *dockerProvisioner) ActionLimiter() provision.ActionLimiter {
	return p.actionLimiter
}

func (p *dockerProvisioner) initAutoScaleConfig() *autoScaleConfig {
	enabled, _ := config.GetBool("docker:auto-scale:enabled")
	waitSecondsNewMachine, _ := config.GetInt("docker:auto-scale:wait-new-time")
	runInterval, _ := config.GetInt("docker:auto-scale:run-interval")
	TotalMemoryMetadata, _ := config.GetString("docker:scheduler:total-memory-metadata")
	return &autoScaleConfig{
		TotalMemoryMetadata: TotalMemoryMetadata,
		WaitTimeNewMachine:  time.Duration(waitSecondsNewMachine) * time.Second,
		RunInterval:         time.Duration(runInterval) * time.Second,
		Enabled:             enabled,
		provisioner:         p,
		done:                make(chan bool),
	}
}

func (p *dockerProvisioner) cloneProvisioner(ignoredContainers []container.Container) (*dockerProvisioner, error) {
	var err error
	overridenProvisioner := *p
	containerIds := make([]string, len(ignoredContainers))
	for i := range ignoredContainers {
		containerIds[i] = ignoredContainers[i].ID
	}
	overridenProvisioner.scheduler = &segregatedScheduler{
		maxMemoryRatio:      p.scheduler.maxMemoryRatio,
		TotalMemoryMetadata: p.scheduler.TotalMemoryMetadata,
		provisioner:         &overridenProvisioner,
		ignoredContainers:   containerIds,
	}
	overridenProvisioner.cluster, err = cluster.New(overridenProvisioner.scheduler, p.storage)
	if err != nil {
		return nil, err
	}
	overridenProvisioner.cluster.Healer = p.cluster.Healer
	return &overridenProvisioner, nil
}

func (p *dockerProvisioner) stopDryMode() {
	if p.isDryMode {
		p.cluster.StopDryMode()
		coll := p.Collection()
		defer coll.Close()
		coll.DropCollection()
	}
}

func (p *dockerProvisioner) dryMode(ignoredContainers []container.Container) (*dockerProvisioner, error) {
	var err error
	overridenProvisioner := &dockerProvisioner{
		collectionName: "containers_dry_" + randomString(),
		isDryMode:      true,
		actionLimiter:  &provision.LocalLimiter{},
	}
	containerIds := make([]string, len(ignoredContainers))
	for i := range ignoredContainers {
		containerIds[i] = ignoredContainers[i].ID
	}
	overridenProvisioner.scheduler = &segregatedScheduler{
		maxMemoryRatio:      p.scheduler.maxMemoryRatio,
		TotalMemoryMetadata: p.scheduler.TotalMemoryMetadata,
		provisioner:         overridenProvisioner,
		ignoredContainers:   containerIds,
	}
	overridenProvisioner.cluster, err = cluster.New(overridenProvisioner.scheduler, p.storage)
	if err != nil {
		return nil, err
	}
	overridenProvisioner.cluster.DryMode()
	containersToCopy, err := p.listAllContainers()
	if err != nil {
		return nil, err
	}
	coll := overridenProvisioner.Collection()
	defer coll.Close()
	toInsert := make([]interface{}, len(containersToCopy))
	for i := range containersToCopy {
		toInsert[i] = containersToCopy[i]
	}
	if len(toInsert) > 0 {
		err = coll.Insert(toInsert...)
		if err != nil {
			return nil, err
		}
	}
	return overridenProvisioner, nil
}

func (p *dockerProvisioner) Cluster() *cluster.Cluster {
	if p.cluster == nil {
		panic("nil cluster")
	}
	return p.cluster
}

func (p *dockerProvisioner) StartupMessage() (string, error) {
	nodeList, err := p.Cluster().UnfilteredNodes()
	if err != nil {
		return "", err
	}
	out := "Docker provisioner reports the following nodes:\n"
	for _, node := range nodeList {
		out += fmt.Sprintf("    Docker node: %s\n", node.Address)
	}
	return out, nil
}

func (p *dockerProvisioner) Initialize() error {
	err := nodecontainer.RegisterQueueTask(p)
	if err != nil {
		return err
	}
	err = registerRoutesRebuildTask()
	if err != nil {
		return err
	}
	return p.initDockerCluster()
}

// Provision creates a route for the container
func (p *dockerProvisioner) Provision(app provision.App) error {
	r, err := getRouterForApp(app)
	if err != nil {
		log.Fatalf("Failed to get router: %s", err)
		return err
	}
	return r.AddBackend(app.GetName())
}

func (p *dockerProvisioner) Restart(a provision.App, process string, w io.Writer) error {
	containers, err := p.listContainersByProcess(a.GetName(), process)
	if err != nil {
		return err
	}
	imageId, err := appCurrentImageName(a.GetName())
	if err != nil {
		return err
	}
	if w == nil {
		w = ioutil.Discard
	}
	writer := io.MultiWriter(w, &app.LogWriter{App: a})
	toAdd := make(map[string]*containersToAdd, len(containers))
	for _, c := range containers {
		if _, ok := toAdd[c.ProcessName]; !ok {
			toAdd[c.ProcessName] = &containersToAdd{Quantity: 0}
		}
		toAdd[c.ProcessName].Quantity++
		toAdd[c.ProcessName].Status = provision.StatusStarted
	}
	_, err = p.runReplaceUnitsPipeline(writer, a, toAdd, containers, imageId)
	routesRebuildOrEnqueue(a.GetName())
	return err
}

func (p *dockerProvisioner) Start(app provision.App, process string) error {
	containers, err := p.listContainersByProcess(app.GetName(), process)
	if err != nil {
		return stderr.New(fmt.Sprintf("Got error while getting app containers: %s", err))
	}
	err = runInContainers(containers, func(c *container.Container, _ chan *container.Container) error {
		startErr := c.Start(&container.StartArgs{
			Provisioner: p,
			App:         app,
		})
		if startErr != nil {
			return startErr
		}
		c.SetStatus(p, provision.StatusStarting, true)
		if info, infoErr := c.NetworkInfo(p); infoErr == nil {
			p.fixContainer(c, info)
		}
		return nil
	}, nil, true)
	routesRebuildOrEnqueue(app.GetName())
	return err
}

func (p *dockerProvisioner) Stop(app provision.App, process string) error {
	containers, err := p.listContainersByProcess(app.GetName(), process)
	if err != nil {
		log.Errorf("Got error while getting app containers: %s", err)
		return nil
	}
	return runInContainers(containers, func(c *container.Container, _ chan *container.Container) error {
		err := c.Stop(p)
		if err != nil {
			log.Errorf("Failed to stop %q: %s", app.GetName(), err)
		}
		return err
	}, nil, true)
}

func (p *dockerProvisioner) Sleep(app provision.App, process string) error {
	containers, err := p.listContainersByProcess(app.GetName(), process)
	if err != nil {
		log.Errorf("Got error while getting app containers: %s", err)
		return nil
	}
	return runInContainers(containers, func(c *container.Container, _ chan *container.Container) error {
		err := c.Sleep(p)
		if err != nil {
			log.Errorf("Failed to sleep %q: %s", app.GetName(), err)
		}
		return err
	}, nil, true)
}

func (p *dockerProvisioner) Swap(app1, app2 provision.App, cnameOnly bool) error {
	r, err := getRouterForApp(app1)
	if err != nil {
		return err
	}
	err = r.Swap(app1.GetName(), app2.GetName(), cnameOnly)
	if err != nil {
		routesRebuildOrEnqueue(app1.GetName())
		routesRebuildOrEnqueue(app2.GetName())
	}
	return err
}

func (p *dockerProvisioner) Rollback(a provision.App, imageId string, w io.Writer) (string, error) {
	if _, err := app.GetImage(a.GetName(), imageId); err != nil {
		return "", stderr.New(fmt.Sprintf("Image %q %q", imageId, err.Error()))
	}
	return imageId, p.deploy(a, imageId, w)
}

func (p *dockerProvisioner) ImageDeploy(app provision.App, imageId string, w io.Writer) (string, error) {
	cluster := p.Cluster()
	if !strings.Contains(imageId, ":") {
		imageId = fmt.Sprintf("%s:latest", imageId)
	}
	fmt.Fprintln(w, "---- Pulling image to tsuru ----")
	pullOpts := docker.PullImageOptions{
		Repository:        imageId,
		OutputStream:      w,
		InactivityTimeout: net.StreamInactivityTimeout,
	}
	nodes, err := cluster.NodesForMetadata(map[string]string{"pool": app.GetPool()})
	if err != nil {
		return "", err
	}
	node, _, err := p.scheduler.minMaxNodes(nodes, app.GetName(), "")
	if err != nil {
		return "", err
	}
	err = cluster.PullImage(pullOpts, docker.AuthConfiguration{}, node)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(w, "---- Getting process from image ----")
	cmd := "cat /home/application/current/Procfile || cat /app/user/Procfile || cat /Procfile"
	output, _ := p.runCommandInContainer(imageId, cmd, app)
	procfile := getProcessesFromProcfile(output.String())
	imageInspect, err := cluster.InspectImage(imageId)
	if err != nil {
		return "", err
	}
	if len(procfile) == 0 {
		fmt.Fprintln(w, "  ---> Procfile not found, trying to get entrypoint")
		if len(imageInspect.Config.Entrypoint) == 0 {
			return "", ErrEntrypointOrProcfileNotFound
		}
		webProcess := imageInspect.Config.Entrypoint[0]
		for _, c := range imageInspect.Config.Entrypoint[1:] {
			webProcess += fmt.Sprintf(" %q", c)
		}
		procfile["web"] = webProcess
	}
	for k, v := range procfile {
		fmt.Fprintf(w, "  ---> Process %s found with command: %v\n", k, v)
	}
	newImage, err := appNewImageName(app.GetName())
	if err != nil {
		return "", err
	}
	imageInfo := strings.Split(newImage, ":")
	err = cluster.TagImage(imageId, docker.TagImageOptions{Repo: strings.Join(imageInfo[:len(imageInfo)-1], ":"), Tag: imageInfo[len(imageInfo)-1], Force: true})
	if err != nil {
		return "", err
	}
	registry, err := config.GetString("docker:registry")
	if err != nil {
		return "", err
	}
	fmt.Fprintln(w, "---- Pushing image to tsuru ----")
	pushOpts := docker.PushImageOptions{
		Name:              strings.Join(imageInfo[:len(imageInfo)-1], ":"),
		Tag:               imageInfo[len(imageInfo)-1],
		Registry:          registry,
		OutputStream:      w,
		InactivityTimeout: net.StreamInactivityTimeout,
	}
	err = cluster.PushImage(pushOpts, mainDockerProvisioner.RegistryAuthConfig())
	if err != nil {
		return "", err
	}
	imageData := createImageMetadata(newImage, procfile)
	if len(imageInspect.Config.ExposedPorts) > 1 {
		return "", stderr.New("Too many ports. You should especify which one you want to.")
	}
	for k := range imageInspect.Config.ExposedPorts {
		imageData.CustomData["exposedPort"] = string(k)
	}
	err = saveImageCustomData(newImage, imageData.CustomData)
	if err != nil {
		return "", err
	}
	app.SetUpdatePlatform(true)
	return newImage, p.deploy(app, newImage, w)
}

func (p *dockerProvisioner) ArchiveDeploy(app provision.App, archiveURL string, w io.Writer) (string, error) {
	imageId, err := p.archiveDeploy(app, p.getBuildImage(app), archiveURL, w)
	if err != nil {
		return "", err
	}
	return imageId, p.deployAndClean(app, imageId, w)
}

func (p *dockerProvisioner) UploadDeploy(app provision.App, archiveFile io.ReadCloser, fileSize int64, build bool, w io.Writer) (string, error) {
	if build {
		return "", stderr.New("running UploadDeploy with build=true is not yet supported")
	}
	dirPath := "/home/application/"
	filePath := fmt.Sprintf("%sarchive.tar.gz", dirPath)
	user, err := config.GetString("docker:user")
	if err != nil {
		user, _ = config.GetString("docker:ssh:user")
	}
	defer archiveFile.Close()
	imageName := p.getBuildImage(app)
	options := docker.CreateContainerOptions{
		Config: &docker.Config{
			AttachStdout: true,
			AttachStderr: true,
			AttachStdin:  true,
			OpenStdin:    true,
			StdinOnce:    true,
			User:         user,
			Image:        imageName,
			Cmd:          []string{"/bin/bash", "-c", "tail -f /dev/null"},
		},
	}
	cluster := p.Cluster()
	schedOpts := &container.SchedulerOpts{
		AppName:       app.GetName(),
		ActionLimiter: p.ActionLimiter(),
	}
	addr, cont, err := cluster.CreateContainerSchedulerOpts(options, schedOpts, net.StreamInactivityTimeout)
	hostAddr := net.URLToHost(addr)
	if schedOpts.LimiterDone != nil {
		schedOpts.LimiterDone()
	}
	if err != nil {
		return "", err
	}
	defer func() {
		done := p.ActionLimiter().Start(hostAddr)
		cluster.RemoveContainer(docker.RemoveContainerOptions{ID: cont.ID, Force: true})
		done()
	}()
	done := p.ActionLimiter().Start(hostAddr)
	err = cluster.StartContainer(cont.ID, nil)
	done()
	if err != nil {
		return "", err
	}
	reader, writer := io.Pipe()
	tarball := tar.NewWriter(writer)
	if err != nil {
		return "", err
	}
	go func() {
		header := tar.Header{
			Name: "archive.tar.gz",
			Mode: 0666,
			Size: fileSize,
		}
		tarball.WriteHeader(&header)
		n, tarErr := io.Copy(tarball, archiveFile)
		if tarErr != nil {
			log.Errorf("upload-deploy: unable to copy archive to tarball: %s", tarErr.Error())
			writer.CloseWithError(tarErr)
			tarball.Close()
			return
		}
		if n != fileSize {
			tarErr = stderr.New("upload-deploy: short-write copying to tarball")
			log.Errorf(tarErr.Error())
			writer.CloseWithError(tarErr)
			tarball.Close()
			return
		}
		tarErr = tarball.Close()
		if tarErr != nil {
			writer.CloseWithError(tarErr)
		}
		writer.Close()
	}()
	uploadOpts := docker.UploadToContainerOptions{
		InputStream: reader,
		Path:        dirPath,
	}
	err = cluster.UploadToContainer(cont.ID, uploadOpts)
	if err != nil {
		return "", err
	}
	done = p.ActionLimiter().Start(hostAddr)
	err = cluster.StopContainer(cont.ID, 10)
	done()
	if err != nil {
		return "", err
	}
	done = p.ActionLimiter().Start(hostAddr)
	image, err := cluster.CommitContainer(docker.CommitContainerOptions{Container: cont.ID})
	done()
	imageId, err := p.archiveDeploy(app, image.ID, "file://"+filePath, w)
	if err != nil {
		return "", err
	}
	return imageId, p.deployAndClean(app, imageId, w)
}

func (p *dockerProvisioner) deployAndClean(a provision.App, imageId string, w io.Writer) error {
	err := p.deploy(a, imageId, w)
	if err != nil {
		p.cleanImage(a.GetName(), imageId)
	}
	return err
}

func (p *dockerProvisioner) deploy(a provision.App, imageId string, w io.Writer) error {
	containers, err := p.listContainersByApp(a.GetName())
	if err != nil {
		return err
	}
	imageData, err := getImageCustomData(imageId)
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		toAdd := make(map[string]*containersToAdd, len(imageData.Processes))
		for processName := range imageData.Processes {
			_, ok := toAdd[processName]
			if !ok {
				ct := containersToAdd{Quantity: 0}
				toAdd[processName] = &ct
			}
			toAdd[processName].Quantity++
		}
		if err = setQuota(a, toAdd); err != nil {
			return err
		}
		_, err = p.runCreateUnitsPipeline(w, a, toAdd, imageId, imageData.ExposedPort)
	} else {
		toAdd := getContainersToAdd(imageData, containers)
		if err = setQuota(a, toAdd); err != nil {
			return err
		}
		_, err = p.runReplaceUnitsPipeline(w, a, toAdd, containers, imageId)
	}
	routesRebuildOrEnqueue(a.GetName())
	return err
}

func setQuota(app provision.App, toAdd map[string]*containersToAdd) error {
	var total int
	for _, ct := range toAdd {
		total += ct.Quantity
	}
	err := app.SetQuotaInUse(total)
	if err != nil {
		return &errors.CompositeError{
			Base:    err,
			Message: "Cannot start application units",
		}
	}
	return nil
}

func getContainersToAdd(data ImageMetadata, oldContainers []container.Container) map[string]*containersToAdd {
	processMap := make(map[string]*containersToAdd, len(data.Processes))
	for name := range data.Processes {
		processMap[name] = &containersToAdd{}
	}
	minCount := 0
	for _, container := range oldContainers {
		if container.ProcessName == "" {
			minCount++
		}
		if _, ok := processMap[container.ProcessName]; ok {
			processMap[container.ProcessName].Quantity++
		}
	}
	if minCount == 0 {
		minCount = 1
	}
	for name, cont := range processMap {
		if cont.Quantity == 0 {
			processMap[name].Quantity = minCount
		}
	}
	return processMap
}

func (p *dockerProvisioner) Destroy(app provision.App) error {
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		log.Errorf("Failed to list app containers: %s", err.Error())
		return err
	}
	args := changeUnitsPipelineArgs{
		app:         app,
		toRemove:    containers,
		writer:      ioutil.Discard,
		provisioner: p,
		appDestroy:  true,
	}
	pipeline := action.NewPipeline(
		&removeOldRoutes,
		&provisionRemoveOldUnits,
		&provisionUnbindOldUnits,
	)
	err = pipeline.Execute(args)
	if err != nil {
		return err
	}
	images, err := listAppImages(app.GetName())
	if err != nil {
		log.Errorf("Failed to get image ids for app %s: %s", app.GetName(), err.Error())
	}
	cluster := p.Cluster()
	for _, imageId := range images {
		err = cluster.RemoveImage(imageId)
		if err != nil {
			log.Errorf("Failed to remove image %s: %s", imageId, err.Error())
		}
		err = cluster.RemoveFromRegistry(imageId)
		if err != nil {
			log.Errorf("Failed to remove image %s from registry: %s", imageId, err.Error())
		}
	}
	err = deleteAllAppImageNames(app.GetName())
	if err != nil {
		log.Errorf("Failed to remove image names from storage for app %s: %s", app.GetName(), err.Error())
	}
	r, err := getRouterForApp(app)
	if err != nil {
		log.Errorf("Failed to get router: %s", err.Error())
		return err
	}
	err = r.RemoveBackend(app.GetName())
	if err != nil {
		log.Errorf("Failed to remove route backend: %s", err.Error())
		return err
	}
	return nil
}

func (*dockerProvisioner) Addr(app provision.App) (string, error) {
	r, err := getRouterForApp(app)
	if err != nil {
		log.Errorf("Failed to get router: %s", err)
		return "", err
	}
	addr, err := r.Addr(app.GetName())
	if err != nil {
		log.Errorf("Failed to obtain app %s address: %s", app.GetName(), err)
		return "", err
	}
	return addr, nil
}

func (p *dockerProvisioner) runRestartAfterHooks(cont *container.Container, w io.Writer) error {
	yamlData, err := getImageTsuruYamlData(cont.Image)
	if err != nil {
		return err
	}
	cmds := yamlData.Hooks.Restart.After
	for _, cmd := range cmds {
		err := cont.Exec(p, w, w, cmd)
		if err != nil {
			return fmt.Errorf("couldn't execute restart:after hook %q(%s): %s", cmd, cont.ShortID(), err.Error())
		}
	}
	return nil
}

func addContainersWithHost(args *changeUnitsPipelineArgs) ([]container.Container, error) {
	a := args.app
	w := args.writer
	var units int
	processMsg := make([]string, 0, len(args.toAdd))
	imageId := args.imageId
	for processName, v := range args.toAdd {
		units += v.Quantity
		if processName == "" {
			_, processName, _ = processCmdForImage(processName, imageId)
		}
		processMsg = append(processMsg, fmt.Sprintf("[%s: %d]", processName, v.Quantity))
	}
	var destinationHost []string
	if args.toHost != "" {
		destinationHost = []string{args.toHost}
	}
	if w == nil {
		w = ioutil.Discard
	}
	fmt.Fprintf(w, "\n---- Starting %d new %s %s ----\n", units, pluralize("unit", units), strings.Join(processMsg, " "))
	oldContainers := make([]container.Container, 0, units)
	for processName, cont := range args.toAdd {
		for i := 0; i < cont.Quantity; i++ {
			oldContainers = append(oldContainers, container.Container{
				ProcessName: processName,
				Status:      cont.Status.String(),
			})
		}
	}
	rollbackCallback := func(c *container.Container) {
		log.Errorf("Removing container %q due failed add units.", c.ID)
		errRem := c.Remove(args.provisioner)
		if errRem != nil {
			log.Errorf("Unable to destroy container %q: %s", c.ID, errRem)
		}
	}
	var (
		createdContainers []*container.Container
		m                 sync.Mutex
	)
	err := runInContainers(oldContainers, func(c *container.Container, toRollback chan *container.Container) error {
		c, startErr := args.provisioner.start(c, a, imageId, w, args.exposedPort, destinationHost...)
		if startErr != nil {
			return startErr
		}
		toRollback <- c
		m.Lock()
		createdContainers = append(createdContainers, c)
		m.Unlock()
		fmt.Fprintf(w, " ---> Started unit %s [%s]\n", c.ShortID(), c.ProcessName)
		return nil
	}, rollbackCallback, true)
	if err != nil {
		return nil, err
	}
	result := make([]container.Container, len(createdContainers))
	i := 0
	for _, c := range createdContainers {
		result[i] = *c
		i++
	}
	return result, nil
}

func (p *dockerProvisioner) AddUnits(a provision.App, units uint, process string, w io.Writer) ([]provision.Unit, error) {
	if a.GetDeploys() == 0 {
		return nil, stderr.New("New units can only be added after the first deployment")
	}
	if units == 0 {
		return nil, stderr.New("Cannot add 0 units")
	}
	if w == nil {
		w = ioutil.Discard
	}
	writer := io.MultiWriter(w, &app.LogWriter{App: a})
	imageId, err := appCurrentImageName(a.GetName())
	if err != nil {
		return nil, err
	}
	imageData, err := getImageCustomData(imageId)
	if err != nil {
		return nil, err
	}
	conts, err := p.runCreateUnitsPipeline(writer, a, map[string]*containersToAdd{process: {Quantity: int(units)}}, imageId, imageData.ExposedPort)
	routesRebuildOrEnqueue(a.GetName())
	if err != nil {
		return nil, err
	}
	result := make([]provision.Unit, len(conts))
	for i, c := range conts {
		result[i] = c.AsUnit(a)
	}
	return result, nil
}

func (p *dockerProvisioner) RemoveUnits(a provision.App, units uint, processName string, w io.Writer) error {
	if a == nil {
		return stderr.New("remove units: app should not be nil")
	}
	if units == 0 {
		return stderr.New("cannot remove zero units")
	}
	var err error
	if w == nil {
		w = ioutil.Discard
	}
	imgId, err := appCurrentImageName(a.GetName())
	if err != nil {
		return err
	}
	_, processName, err = processCmdForImage(processName, imgId)
	if err != nil {
		return err
	}
	containers, err := p.listContainersByProcess(a.GetName(), processName)
	if err != nil {
		return err
	}
	if len(containers) < int(units) {
		return fmt.Errorf("cannot remove %d units from process %q, only %d available", units, processName, len(containers))
	}
	fmt.Fprintf(w, "\n---- Removing %d %s ----\n", units, pluralize("unit", int(units)))
	p, err = p.cloneProvisioner(nil)
	if err != nil {
		return err
	}
	toRemove := make([]container.Container, 0, units)
	for i := 0; i < int(units); i++ {
		var (
			containerID string
			cont        *container.Container
		)
		containerID, err = p.scheduler.GetRemovableContainer(a.GetName(), processName)
		if err != nil {
			return err
		}
		cont, err = p.GetContainer(containerID)
		if err != nil {
			return err
		}
		p.scheduler.ignoredContainers = append(p.scheduler.ignoredContainers, cont.ID)
		toRemove = append(toRemove, *cont)
	}
	args := changeUnitsPipelineArgs{
		app:         a,
		toRemove:    toRemove,
		writer:      w,
		provisioner: p,
	}
	pipeline := action.NewPipeline(
		&removeOldRoutes,
		&provisionRemoveOldUnits,
		&provisionUnbindOldUnits,
	)
	err = pipeline.Execute(args)
	if err != nil {
		return fmt.Errorf("error removing routes, units weren't removed: %s", err)
	}
	return nil
}

func (p *dockerProvisioner) SetUnitStatus(unit provision.Unit, status provision.Status) error {
	cont, err := p.GetContainer(unit.ID)
	if _, ok := err.(*provision.UnitNotFoundError); ok && unit.Name != "" {
		cont, err = p.GetContainerByName(unit.Name)
	}
	if err != nil {
		return err
	}
	if cont.Status == provision.StatusBuilding.String() || cont.Status == provision.StatusAsleep.String() {
		return nil
	}
	if status == provision.StatusStopped && cont.Status != provision.StatusStopped.String() {
		status = provision.StatusError
	}
	if unit.AppName != "" && cont.AppName != unit.AppName {
		return stderr.New("wrong app name")
	}
	err = cont.SetStatus(p, status, true)
	if err != nil {
		return err
	}
	return p.checkContainer(cont)
}

func (p *dockerProvisioner) ExecuteCommandOnce(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	containers, err := p.listRunnableContainersByApp(app.GetName())
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return provision.ErrEmptyApp
	}
	container := containers[0]
	return container.Exec(p, stdout, stderr, cmd, args...)
}

func (p *dockerProvisioner) ExecuteCommand(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	containers, err := p.listRunnableContainersByApp(app.GetName())
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return provision.ErrEmptyApp
	}
	for _, c := range containers {
		err = c.Exec(p, stdout, stderr, cmd, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *dockerProvisioner) SetCName(app provision.App, cname string) error {
	r, err := getRouterForApp(app)
	if err != nil {
		return err
	}
	err = r.SetCName(cname, app.GetName())
	if err != nil {
		routesRebuildOrEnqueue(app.GetName())
	}
	return err
}

func (p *dockerProvisioner) UnsetCName(app provision.App, cname string) error {
	r, err := getRouterForApp(app)
	if err != nil {
		return err
	}
	err = r.UnsetCName(cname, app.GetName())
	if err != nil {
		routesRebuildOrEnqueue(app.GetName())
	}
	return err
}

func (p *dockerProvisioner) AdminCommands() []cmd.Command {
	return []cmd.Command{
		&moveContainerCmd{},
		&moveContainersCmd{},
		&rebalanceContainersCmd{},
		&addNodeToSchedulerCmd{},
		&removeNodeFromSchedulerCmd{},
		&listNodesInTheSchedulerCmd{},
		&healer.ListHealingHistoryCmd{},
		&healer.GetNodeHealingConfigCmd{},
		&healer.SetNodeHealingConfigCmd{},
		&healer.DeleteNodeHealingConfigCmd{},
		&autoScaleRunCmd{},
		&listAutoScaleHistoryCmd{},
		&autoScaleInfoCmd{},
		&autoScaleSetRuleCmd{},
		&autoScaleDeleteRuleCmd{},
		&updateNodeToSchedulerCmd{},
		&dockerLogInfo{},
		&dockerLogUpdate{},
		&nodecontainer.NodeContainerList{},
		&nodecontainer.NodeContainerAdd{},
		&nodecontainer.NodeContainerInfo{},
		&nodecontainer.NodeContainerUpdate{},
		&nodecontainer.NodeContainerDelete{},
		&nodecontainer.NodeContainerUpgrade{},
		&cmd.RemovedCommand{Name: "bs-env-set", Help: "You should use `tsuru-admin node-container-update big-sibling` instead."},
		&cmd.RemovedCommand{Name: "bs-info", Help: "You should use `tsuru-admin node-container-info big-sibling` instead."},
		&cmd.RemovedCommand{Name: "bs-upgrade", Help: "You should use `tsuru-admin node-container-upgrade big-sibling` instead."},
	}
}

func (p *dockerProvisioner) Collection() *storage.Collection {
	conn, err := db.Conn()
	if err != nil {
		log.Errorf("Failed to connect to the database: %s", err)
	}
	return conn.Collection(p.collectionName)
}

// PlatformAdd build and push a new docker platform to register
func (p *dockerProvisioner) PlatformAdd(opts provision.PlatformOptions) error {
	return p.buildPlatform(opts.Name, opts.Args, opts.Output, opts.Input)
}

func (p *dockerProvisioner) PlatformUpdate(opts provision.PlatformOptions) error {
	return p.buildPlatform(opts.Name, opts.Args, opts.Output, opts.Input)
}

func (p *dockerProvisioner) buildPlatform(name string, args map[string]string, w io.Writer, r io.Reader) error {
	var inputStream io.Reader
	var dockerfileURL string
	if r != nil {
		data, err := ioutil.ReadAll(r)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		writer := tar.NewWriter(&buf)
		writer.WriteHeader(&tar.Header{
			Name: "Dockerfile",
			Mode: 0644,
			Size: int64(len(data)),
		})
		writer.Write(data)
		writer.Close()
		inputStream = &buf
	} else {
		dockerfileURL = args["dockerfile"]
		if dockerfileURL == "" {
			return stderr.New("Dockerfile is required")
		}
		if _, err := url.ParseRequestURI(dockerfileURL); err != nil {
			return stderr.New("dockerfile parameter must be a URL")
		}
	}
	imageName := platformImageName(name)
	cluster := p.Cluster()
	buildOptions := docker.BuildImageOptions{
		Name:              imageName,
		Pull:              true,
		NoCache:           true,
		RmTmpContainer:    true,
		Remote:            dockerfileURL,
		InputStream:       inputStream,
		OutputStream:      w,
		InactivityTimeout: net.StreamInactivityTimeout,
	}
	err := cluster.BuildImage(buildOptions)
	if err != nil {
		return err
	}
	parts := strings.Split(imageName, ":")
	var tag string
	if len(parts) > 2 {
		imageName = strings.Join(parts[:len(parts)-1], ":")
		tag = parts[len(parts)-1]
	} else if len(parts) > 1 {
		imageName = parts[0]
		tag = parts[1]
	} else {
		imageName = parts[0]
		tag = "latest"
	}
	return p.PushImage(imageName, tag)
}

func (p *dockerProvisioner) PlatformRemove(name string) error {
	err := p.Cluster().RemoveImage(platformImageName(name))
	if err != nil && err == docker.ErrNoSuchImage {
		log.Errorf("error on remove image %s from docker.", name)
		return nil
	}
	return err
}

func (p *dockerProvisioner) Units(app provision.App) ([]provision.Unit, error) {
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		return nil, err
	}
	units := make([]provision.Unit, len(containers))
	for i, container := range containers {
		units[i] = container.AsUnit(app)
	}
	return units, nil
}

func (p *dockerProvisioner) RoutableUnits(app provision.App) ([]provision.Unit, error) {
	imageId, err := appCurrentImageName(app.GetName())
	if err != nil && err != errNoImagesAvailable {
		return nil, err
	}
	webProcessName, err := getImageWebProcessName(imageId)
	if err != nil {
		return nil, err
	}
	containers, err := p.listContainersByApp(app.GetName())
	if err != nil {
		return nil, err
	}
	units := make([]provision.Unit, 0, len(containers))
	for _, container := range containers {
		if container.ProcessName == webProcessName && container.ValidAddr() {
			units = append(units, container.AsUnit(app))
		}
	}
	return units, nil
}

func (p *dockerProvisioner) RegisterUnit(unit provision.Unit, customData map[string]interface{}) error {
	cont, err := p.GetContainer(unit.ID)
	if err != nil {
		return err
	}
	if cont.Status == provision.StatusBuilding.String() {
		if cont.BuildingImage != "" && customData != nil {
			return saveImageCustomData(cont.BuildingImage, customData)
		}
		return nil
	}
	err = cont.SetStatus(p, provision.StatusStarted, true)
	if err != nil {
		return err
	}
	return p.checkContainer(cont)
}

func (p *dockerProvisioner) Shell(opts provision.ShellOptions) error {
	var (
		c   *container.Container
		err error
	)
	if opts.Unit != "" {
		c, err = p.GetContainer(opts.Unit)
	} else {
		c, err = p.getOneContainerByAppName(opts.App.GetName())
	}
	if err != nil {
		return err
	}
	return c.Shell(p, opts.Conn, opts.Conn, opts.Conn, container.Pty{Width: opts.Width, Height: opts.Height, Term: opts.Term})
}

func (p *dockerProvisioner) ValidAppImages(appName string) ([]string, error) {
	return listValidAppImages(appName)
}

func (p *dockerProvisioner) Nodes(app provision.App) ([]cluster.Node, error) {
	pool := app.GetPool()
	var (
		pools []provision.Pool
		err   error
	)
	if pool == "" {
		pools, err = provision.ListPools(bson.M{"$or": []bson.M{{"teams": app.GetTeamOwner()}, {"teams": bson.M{"$in": app.GetTeamsName()}}}})
	} else {
		pools, err = provision.ListPools(bson.M{"_id": pool})
	}
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		query := bson.M{"default": true}
		pools, err = provision.ListPools(query)
		if err != nil {
			return nil, err
		}
	}
	if len(pools) == 0 {
		return nil, errNoDefaultPool
	}
	for _, pool := range pools {
		nodes, err := p.Cluster().NodesForMetadata(map[string]string{"pool": pool.Name})
		if err != nil {
			return nil, errNoDefaultPool
		}
		if len(nodes) > 0 {
			return nodes, nil
		}
	}
	var nameList []string
	for _, pool := range pools {
		nameList = append(nameList, pool.Name)
	}
	poolsStr := strings.Join(nameList, ", pool=")
	return nil, fmt.Errorf("No nodes found with one of the following metadata: pool=%s", poolsStr)
}

func (p *dockerProvisioner) MetricEnvs(app provision.App) map[string]string {
	bsContainer, err := nodecontainer.LoadNodeContainer(app.GetPool(), nodecontainer.BsDefaultName)
	if err != nil {
		return map[string]string{}
	}
	envs := bsContainer.EnvMap()
	for envName := range envs {
		if !strings.HasPrefix(envName, "METRICS_") {
			delete(envs, envName)
		}
	}
	return envs
}

func (p *dockerProvisioner) LogsEnabled(app provision.App) (bool, string, error) {
	const (
		logBackendsEnv      = "LOG_BACKENDS"
		logDocKeyFormat     = "LOG_%s_DOC"
		tsuruLogBackendName = "tsuru"
	)
	isBS, err := container.LogIsBS(app.GetPool())
	if err != nil {
		return false, "", err
	}
	if !isBS {
		driver, _, _ := container.LogOpts(app.GetPool())
		msg := fmt.Sprintf("Logs not available through tsuru. Enabled log driver is %q.", driver)
		return false, msg, nil
	}
	bsContainer, err := nodecontainer.LoadNodeContainer(app.GetPool(), nodecontainer.BsDefaultName)
	if err != nil {
		return false, "", err
	}
	envs := bsContainer.EnvMap()
	enabledBackends := envs[logBackendsEnv]
	if enabledBackends == "" {
		return true, "", nil
	}
	backendsList := strings.Split(enabledBackends, ",")
	for i := range backendsList {
		backendsList[i] = strings.TrimSpace(backendsList[i])
		if backendsList[i] == tsuruLogBackendName {
			return true, "", nil
		}
	}
	var docs []string
	for _, backendName := range backendsList {
		keyName := fmt.Sprintf(logDocKeyFormat, strings.ToUpper(backendName))
		backendDoc := envs[keyName]
		var docLine string
		if backendDoc == "" {
			docLine = fmt.Sprintf("* %s", backendName)
		} else {
			docLine = fmt.Sprintf("* %s: %s", backendName, backendDoc)
		}
		docs = append(docs, docLine)
	}
	fullDoc := fmt.Sprintf("Logs not available through tsuru. Enabled log backends are:\n%s",
		strings.Join(docs, "\n"))
	return false, fullDoc, nil
}

func pluralize(str string, sz int) string {
	if sz == 0 || sz > 1 {
		str = str + "s"
	}
	return str
}

func (p *dockerProvisioner) FilterAppsByUnitStatus(apps []provision.App, status []string) ([]provision.App, error) {
	if apps == nil {
		return nil, fmt.Errorf("apps must be provided to FilterAppsByUnitStatus")
	}
	if status == nil {
		return make([]provision.App, 0), nil
	}
	appNames := make([]string, len(apps))
	for i, app := range apps {
		appNames[i] = app.GetName()
	}
	containers, err := p.listContainersByAppAndStatus(appNames, status)
	if err != nil {
		return nil, err
	}
	result := make([]provision.App, 0)
	for _, app := range apps {
		for _, c := range containers {
			if app.GetName() == c.AppName {
				result = append(result, app)
				break
			}
		}
	}
	return result, nil
}

func (p *dockerProvisioner) SetNodeStatus(nodeData provision.NodeStatusData) error {
	if p.nodeHealer == nil {
		return nil
	}
	return p.nodeHealer.UpdateNodeData(nodeData)
}
