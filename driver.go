package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/gommon/log"
	"github.com/luthermonson/go-proxmox"

	"github.com/rancher/machine/libmachine/drivers"
	"github.com/rancher/machine/libmachine/mcnflag"
	"github.com/rancher/machine/libmachine/ssh"
	"github.com/rancher/machine/libmachine/state"
)

// Driver for Proxmox VE
type Driver struct {
	*drivers.BaseDriver
	client *proxmox.Client

	// Basic Authentication for Proxmox VE
	Host     string // Host to connect to
	Port     string // Port to connect to (default 8006)
	Node     string // optional, node to create VM on, host used if omitted but must match internal node name
	User     string // username
	Password string // password
	Realm    string // realm, e.g. pam, pve, etc.

	// File to load as boot image RancherOS/Boot2Docker
	ImageFile string // in the format <storagename>:iso/<filename>.iso

	Pool            string // pool to add the VM to (necessary for users with only pool permission)
	Storage         string // internal PVE storage name
	StorageType     string // Type of the storage (currently QCOW2 and RAW)
	DiskSize        string // disk size in GB
	Memory          int    // memory in GB
	StorageFilename string
	Onboot          string // Specifies whether a VM will be started during system bootup.
	Protection      string // Sets the protection flag of the VM. This will disable the remove VM and remove disk operations.
	Citype          string // Specifies the cloud-init configuration format.
	NUMA            string // Enable/disable NUMA

	NetModel    string // Net Interface Model, [e1000, virtio, realtek, etc...]
	NetFirewall string // Enable/disable firewall
	NetMtu      string // set nic MTU
	NetBridge   string // bridge applied to network interface
	NetVlanTag  int    // vlan tag

	ScsiController string
	ScsiAttributes string

	VMID          string // VM ID only filled by create()
	VMID_int      int    // Same as VMID but int
	VMIDRange     string // acceptable range of VMIDs
	CloneVMID     string // VM ID to clone
	CloneFull     int    // Make a full (detached) clone from parent (defaults to true if VMID is not a template, otherwise false)
	GuestUsername string // user to log into the guest OS to copy the public key
	GuestPassword string // password to log into the guest OS to copy the public key
	GuestSSHPort  int    // ssh port to log into the guest OS to copy the public key
	CPU           string // Emulated CPU type.
	CPUSockets    string // The number of cpu sockets.
	CPUCores      string // The number of cores per socket.
	driverDebug   bool   // driver debugging
}

// NewDriver returns a new driver
func NewDriver(hostName, storePath string) drivers.Driver {
	return &Driver{
		BaseDriver: &drivers.BaseDriver{
			SSHUser:     "docker",
			MachineName: hostName,
			StorePath:   storePath,
		},
		Citype: "nocloud", // default to nocloud since this driver will only support linux
	}
}

func (d *Driver) debugf(format string, v ...interface{}) {
	if d.driverDebug {
		log.Infof(format, v...)
	}
}

func (d *Driver) debug(v ...interface{}) {
	if d.driverDebug {
		log.Info(v...)
	}
}

func (d *Driver) connectApi() (client *proxmox.Client, err error) {
	var options []proxmox.Option

	options = append(options, proxmox.WithHTTPClient(&http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}))
	credentials := proxmox.Credentials{
		Username: d.User,
		Password: d.Password,
		Realm:    d.Realm,
	}
	options = append(options, proxmox.WithCredentials(&credentials))

	proxmoxUrl := fmt.Sprintf("https://%s:%s/api2/json", d.Host, d.Port)
	log.Debug(fmt.Sprintf("Connecting to %s", proxmoxUrl))
	d.client = proxmox.NewClient(proxmoxUrl, options...)

	version, err := d.client.Version(context.Background())
	if err != nil {
		return nil, err
	}
	c, err2 := d.client.Cluster(context.Background())
	if err2 != nil {
		return nil, err2
	}

	log.Infof("Connected to pve cluster %s with version: %s", c.Name, version.Version)

	return d.client, err
}

// GetCreateFlags returns the argument flags for the program
func (d *Driver) GetCreateFlags() []mcnflag.Flag {
	return []mcnflag.Flag{
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_HOST",
			Name:   "proxmoxve-proxmox-host",
			Usage:  "Host to connect to",
			Value:  "192.168.1.253",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_PORT",
			Name:   "proxmoxve-proxmox-port",
			Usage:  "Port to connect to",
			Value:  "8006",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_NODE",
			Name:   "proxmoxve-proxmox-node",
			Usage:  "Node to use (defaults to host)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_USER_NAME",
			Name:   "proxmoxve-proxmox-user-name",
			Usage:  "User to connect as",
			Value:  "root",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_USER_PASSWORD",
			Name:   "proxmoxve-proxmox-user-password",
			Usage:  "Password to connect with",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_REALM",
			Name:   "proxmoxve-proxmox-realm",
			Usage:  "Realm to connect to (default: pam)",
			Value:  "pam",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_PROXMOX_POOL",
			Name:   "proxmoxve-proxmox-pool",
			Usage:  "pool to attach to",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_VMID_RANGE",
			Name:   "proxmoxve-vm-vmid-range",
			Usage:  "range of acceptable vmid values <low>[:<high>]",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_STORAGE_PATH",
			Name:   "proxmoxve-vm-storage-path",
			Usage:  "storage to create the VM volume on",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_STORAGE_SIZE",
			Name:   "proxmoxve-vm-storage-size",
			Usage:  "disk size in GB",
			Value:  "16",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_STORAGE_TYPE",
			Name:   "proxmoxve-vm-storage-type",
			Usage:  "storage type to use (QCOW2 or RAW)",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_SCSI_CONTROLLER",
			Name:   "proxmoxve-vm-scsi-controller",
			Usage:  "scsi controller model (default: virtio-scsi-pci)",
			Value:  "virtio-scsi-pci",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_SCSI_ATTRIBUTES",
			Name:   "proxmoxve-vm-scsi-attributes",
			Usage:  "scsi0 attributes",
			Value:  "",
		},
		mcnflag.IntFlag{
			EnvVar: "PROXMOXVE_VM_MEMORY",
			Name:   "proxmoxve-vm-memory",
			Usage:  "memory in GB",
			Value:  8,
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_NUMA",
			Name:   "proxmoxve-vm-numa",
			Usage:  "enable/disable NUMA",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_CPU",
			Name:   "proxmoxve-vm-cpu",
			Usage:  "Emulatd CPU",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_CPU_SOCKETS",
			Name:   "proxmoxve-vm-cpu-sockets",
			Usage:  "number of cpus",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_CPU_CORES",
			Name:   "proxmoxve-vm-cpu-cores",
			Usage:  "number of cpu cores",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_CLONE_VMID",
			Name:   "proxmoxve-vm-clone-vmid",
			Usage:  "vmid to clone",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_START_ONBOOT",
			Name:   "proxmoxve-vm-start-onboot",
			Usage:  "make the VM start automatically onboot (0=false, 1=true, ''=default)",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_PROTECTION",
			Name:   "proxmoxve-vm-protection",
			Usage:  "protect the VM and disks from removal (0=false, 1=true, ''=default)",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_IMAGE_FILE",
			Name:   "proxmoxve-vm-image-file",
			Usage:  "storage of the image file (e.g. local:iso/rancheros-proxmoxve-autoformat.iso)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_NET_MODEL",
			Name:   "proxmoxve-vm-net-model",
			Usage:  "Net Interface model, default virtio (e1000, virtio, realtek, etc...)",
			Value:  "virtio",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_NET_FIREWALL",
			Name:   "proxmoxve-vm-net-firewall",
			Usage:  "enable/disable firewall (0=false, 1=true, ''=default)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_NET_MTU",
			Name:   "proxmoxve-vm-net-mtu",
			Usage:  "set nic mtu (''=default)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_VM_NET_BRIDGE",
			Name:   "proxmoxve-vm-net-bridge",
			Usage:  "bridge to attach network to",
			Value:  "", // leave the flag default value blank to support the clone default behavior if not explicity set of 'use what is most appropriate'
		},
		mcnflag.IntFlag{
			EnvVar: "PROXMOXVE_VM_NET_TAG",
			Name:   "proxmoxve-vm-net-tag",
			Usage:  "vlan tag",
			Value:  0,
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_SSH_USERNAME",
			Name:   "proxmoxve-ssh-username",
			Usage:  "Username to log in to the guest OS (default docker for rancheros)",
			Value:  "",
		},
		mcnflag.StringFlag{
			EnvVar: "PROXMOXVE_SSH_PASSWORD",
			Name:   "proxmoxve-ssh-password",
			Usage:  "Password to log in to the guest OS (default tcuser for rancheros)",
			Value:  "",
		},
		mcnflag.IntFlag{
			EnvVar: "PROXMOXVE_SSH_PORT",
			Name:   "proxmoxve-ssh-port",
			Usage:  "SSH port in the guest to log in to (defaults to 22)",
			Value:  22,
		},
		mcnflag.BoolFlag{
			EnvVar: "PROXMOXVE_DEBUG_DRIVER",
			Name:   "proxmoxve-debug-driver",
			Usage:  "enables debugging in the driver",
		},
	}
}

// DriverName returns the name of the driver
func (d *Driver) DriverName() string {
	return "proxmoxve"
}

// SetConfigFromFlags configures all command line arguments
func (d *Driver) SetConfigFromFlags(flags drivers.DriverOptions) error {
	d.debug("SetConfigFromFlags called")

	// PROXMOX API Connection settings
	d.Host = flags.String("proxmoxve-proxmox-host")
	d.Port = flags.String("proxmoxve-proxmox-port")
	if len(d.Port) == 0 {
		d.Port = "8006"
	}
	d.Node = flags.String("proxmoxve-proxmox-node")
	if len(d.Node) == 0 {
		d.Node = d.Host
	}
	d.User = flags.String("proxmoxve-proxmox-user-name")
	d.Password = flags.String("proxmoxve-proxmox-user-password")
	d.Realm = flags.String("proxmoxve-proxmox-realm")
	d.Pool = flags.String("proxmoxve-proxmox-pool")

	// VM configuration
	d.DiskSize = flags.String("proxmoxve-vm-storage-size")
	d.Storage = flags.String("proxmoxve-vm-storage-path")
	d.StorageType = strings.ToLower(flags.String("proxmoxve-vm-storage-type"))
	d.Memory = flags.Int("proxmoxve-vm-memory")
	d.Memory *= 1024
	d.VMIDRange = flags.String("proxmoxve-vm-vmid-range")
	d.CloneVMID = flags.String("proxmoxve-vm-clone-vmid")
	d.Onboot = flags.String("proxmoxve-vm-start-onboot")
	d.Protection = flags.String("proxmoxve-vm-protection")
	d.ImageFile = flags.String("proxmoxve-vm-image-file")
	d.CPUSockets = flags.String("proxmoxve-vm-cpu-sockets")
	d.CPU = flags.String("proxmoxve-vm-cpu")
	d.CPUCores = flags.String("proxmoxve-vm-cpu-cores")
	d.NetModel = flags.String("proxmoxve-vm-net-model")
	d.NetFirewall = flags.String("proxmoxve-vm-net-firewall")
	d.NetMtu = flags.String("proxmoxve-vm-net-mtu")
	d.NetBridge = flags.String("proxmoxve-vm-net-bridge")
	d.NetVlanTag = flags.Int("proxmoxve-vm-net-tag")
	d.ScsiController = flags.String("proxmoxve-vm-scsi-controller")
	d.ScsiAttributes = flags.String("proxmoxve-vm-scsi-attributes")
	d.driverDebug = flags.Bool("proxmoxve-debug-driver")

	//SSH connection settings
	d.GuestSSHPort = flags.Int("proxmoxve-ssh-port")
	d.GuestUsername = flags.String("proxmoxve-ssh-username")
	d.GuestPassword = flags.String("proxmoxve-ssh-password")

	return nil
}

// GetURL returns the URL for the target docker daemon
func (d *Driver) GetURL() (string, error) {
	ip, err := d.GetIP()
	if err != nil {
		return "", err
	}
	if ip == "" {
		return "", nil
	}
	return fmt.Sprintf("tcp://%s:2376", ip), nil
}

// GetMachineName returns the machine name
func (d *Driver) GetMachineName() string {
	return d.MachineName
}

// GetNetBridge returns the bridge
func (d *Driver) GetNetBridge() string {
	return d.NetBridge
}

// GetNetVlanTag returns the vlan tag
func (d *Driver) GetNetVlanTag() int {
	return d.NetVlanTag
}

func (d *Driver) GetNode() (*proxmox.Node, error) {
	if d.client == nil {
		client, err := d.connectApi()
		if err != nil {
			return nil, err
		}
		d.client = client
	}

	n, err := d.client.Node(context.Background(), d.Node)
	if err != nil {
		return nil, err
	}
	return n, err
}

func (d *Driver) ConfigureVM(name string, value string) error {
	d.debugf("ConfigureVM: %s %s", name, value)
	vm, err := d.GetVM()
	if err != nil {
		return err
	}
	var config proxmox.VirtualMachineOption
	config.Name = name
	config.Value = value

	configTask, err2 := vm.Config(context.Background(), config)

	if err2 != nil {
		return err2
	}

	// wait for the config task
	if err4 := configTask.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err4 != nil {
		return err4
	}

	d.debugf("Config task finished")

	return nil
}

func (d *Driver) OperateVM(operation string) error {

	vm, err := d.GetVM()
	if err != nil {
		return err
	}

	switch operation {
	case "start":
		task, err2 := vm.Start(context.Background())
		log.Debug(task.ID)
		if err2 != nil {
			return err2
		}
		// wait for the task
		if err3 := task.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err3 != nil {
			return err3
		}
	case "stop":
		task, err2 := vm.Stop(context.Background())
		log.Debug(task.ID)
		if err2 != nil {
			return err2
		}
		// wait for the task
		if err3 := task.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err3 != nil {
			return err3
		}
	case "kill":
		task, err2 := vm.Stop(context.Background())
		log.Debug(task.ID)
		if err2 != nil {
			return err2
		}
		// wait for the task
		if err3 := task.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err3 != nil {
			return err3
		}
	case "restart":
		task, err2 := vm.Reset(context.Background())
		log.Debug(task.ID)
		if err2 != nil {
			return err2
		}
		// wait for the task
		if err3 := task.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err3 != nil {
			return err3
		}
	default:
		return errors.New("Invalid operation: " + operation)
	}

	return err

}

func (d *Driver) GetVM() (*proxmox.VirtualMachine, error) {
	if len(d.VMID) < 1 {
		return nil, errors.New("invalid VMID")
	}

	n, err := d.GetNode()
	if err != nil {
		return nil, err
	}
	vm, err2 := n.VirtualMachine(context.Background(), d.VMID_int)
	if err2 != nil {
		return nil, err2
	}
	return vm, err
}

// GetIP returns the ip
func (d *Driver) GetIP() (string, error) {
	vm, err := d.GetVM()

	if err := vm.WaitForAgent(context.Background(), 300); err != nil {
		return "", err
	}
	net := vm.VirtualMachineConfig.Net0
	iFaces, err3 := vm.AgentGetNetworkIFaces(context.Background())
	if err3 != nil {
		return "", err3
	}
	for _, iface := range iFaces {
		if strings.Contains(strings.ToLower(net), strings.ToLower(iface.HardwareAddress)) {
			for _, ip := range iface.IPAddresses {
				if ip.IPAddressType == "ipv4" {
					d.IPAddress = ip.IPAddress
				}
			}
		}
	}

	if d.IPAddress == "" {
		return "", err
	}

	return d.IPAddress, err
}

// GetSSHHostname returns the ssh host returned by the API
func (d *Driver) GetSSHHostname() (string, error) {
	return d.GetIP()
}

// GetSSHPort returns the ssh port, 22 if not specified
func (d *Driver) GetSSHPort() (int, error) {
	return d.GuestSSHPort, nil
}

// GetSSHUsername returns the ssh user name, root if not specified
func (d *Driver) GetSSHUsername() string {
	return d.GuestUsername
}

// GetState returns the state of the VM
func (d *Driver) GetState() (state.State, error) {
	vm, err := d.GetVM()
	if err != nil {
		return state.None, err
	}

	if err := vm.Ping(context.Background()); err != nil {
		return state.None, err
	}

	if vm.IsStopped() {
		return state.Stopped, nil
	}

	if vm.IsRunning() {
		return state.Running, nil
	}

	return state.None, nil
}

// PreCreateCheck is called to enforce pre-creation steps
func (d *Driver) PreCreateCheck() error {

	if d.client == nil {
		client, err := d.connectApi()
		if err != nil {
			return err
		}
		d.client = client
	}

	return nil
}

// Create creates a new VM with storage
func (d *Driver) Create() error {

	newId, err6 := d.GetVmidInRange()
	if err6 != nil {
		return err6
	}

	clone := &proxmox.VirtualMachineCloneOptions{
		Name:    d.MachineName,
		Full:    1,
		Pool:    d.Pool,
		Format:  d.StorageType,
		Storage: d.Storage,
		NewID:   newId,
	}

	d.debugf("cloning new vm from template id '%s'", d.CloneVMID)

	node, err := d.client.Node(context.Background(), d.Node)
	if err != nil {
		return err
	}

	cloneVmId, err := strconv.Atoi(d.CloneVMID)
	if err != nil {
		return err
	}

	clonevm, err := node.VirtualMachine(context.Background(), cloneVmId)
	if err != nil {
		return err
	}

	_, task, err := clonevm.Clone(context.Background(), clone)
	d.debugf("clone task for new vmid '%d' created. newId", newId)

	if err != nil {
		return err
	}

	// wait for the clone task
	if err := task.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err != nil {
		return err
	}
	d.debugf("clone finished for vmid '%d'", newId)

	// explicity set vmid after clone completion to be sure
	d.VMID = fmt.Sprint(newId)
	d.VMID_int = newId

	d.debugf("vmid values VMID: '%s' VMID_int: '%d'", d.VMID, d.VMID_int)

	// resize
	d.debugf("resizing disk '%s' on vmid '%s' to '%s'", "scsi0", d.VMID, d.DiskSize+"G")

	vm, err4 := d.GetVM()
	if err4 != nil {
		return err4
	}
	err5 := vm.ResizeDisk(context.Background(), "scsi0", d.DiskSize+"G")
	if err5 != nil {
		return err5
	}

	d.debugf("add misc configuration options")

	d.ConfigureVM("agent", "1")
	d.ConfigureVM("autostart", "1")
	d.ConfigureVM("memory", fmt.Sprint(d.Memory))
	d.ConfigureVM("sockets", d.CPUSockets)
	d.ConfigureVM("cores", d.CPUCores)
	d.ConfigureVM("kvm", "1")
	d.ConfigureVM("citype", d.Citype)
	d.ConfigureVM("onboot", d.Onboot)
	d.ConfigureVM("protection", d.Protection)

	if len(d.NetBridge) > 0 {
		d.ConfigureVM("Net0", d.generateNetString())
	}

	if len(d.NUMA) > 0 {
		d.ConfigureVM("NUMA", d.NUMA)
	}

	if len(d.CPU) > 0 {
		d.ConfigureVM("CPU", d.CPU)
	}

	// append newly minted ssh key to existing (if any)
	d.appendVmSshKeys()

	// start the VM
	err = d.Start()
	if err != nil {
		return err
	}

	// wait for the agent and get the IPAddress
	vmIp, err := d.GetIP()
	if err != nil {
		return err
	}

	d.debugf("VM got an IP: %s", vmIp)

	return nil
}

func (d *Driver) appendVmSshKeys() error {
	// create and save a new SSH key pair
	d.debug("creating new ssh keypair")
	key, err := d.createSSHKey()
	if err != nil {
		return err
	}

	// !! Workaround for MC-7982.
	key = strings.TrimSpace(key)
	key = fmt.Sprintf("%s %s-%d", key, d.MachineName, time.Now().Unix())
	// !! End workaround for MC-7982.

	var SSHKeys string

	vm, err := d.GetVM()
	if err != nil {
		return err
	}

	if len(vm.VirtualMachineConfig.SSHKeys) > 0 {
		SSHKeys, err = url.QueryUnescape(vm.VirtualMachineConfig.SSHKeys)
		if err != nil {
			return err
		}

		SSHKeys = strings.TrimSpace(SSHKeys)
		SSHKeys += "\n"
	}

	SSHKeys += key
	SSHKeys = strings.TrimSpace(SSHKeys)

	// specially handle setting sshkeys
	// https://forum.proxmox.com/threads/how-to-use-pvesh-set-vms-sshkeys.52570/

	d.debugf("retrieving existing cloud-init sshkeys from vmid '%s'", d.VMID)

	r := strings.NewReplacer("+", "%2B", "=", "%3D", "@", "%40")

	SSHKeys = url.PathEscape(SSHKeys)
	SSHKeys = r.Replace(SSHKeys)

	err3 := d.ConfigureVM("sshkeys", SSHKeys)
	d.debugf("cloud-init sshkeys set to '%s'", SSHKeys)
	if err3 != nil {
		return err3
	}

	return nil
}

func (d *Driver) generateNetString() string {
	var net string = fmt.Sprintf("model=%s,bridge=%s", d.NetModel, d.NetBridge)
	if d.NetVlanTag != 0 {
		net = fmt.Sprintf(net+",tag=%d", d.NetVlanTag)
	}

	if len(d.NetFirewall) > 0 {
		net = fmt.Sprintf(net+",firewall=%s", d.NetFirewall)
	}

	if len(d.NetMtu) > 0 {
		net = fmt.Sprintf(net+",mtu=%s", d.NetMtu)
	}

	return net
}

// Start starts the VM
func (d *Driver) Start() error {
	return d.OperateVM("start")
}

// Stop stopps the VM
func (d *Driver) Stop() error {
	return d.OperateVM("stop")
}

// Restart restarts the VM
func (d *Driver) Restart() error {
	return d.OperateVM("restart")
}

// Kill the VM immediately
func (d *Driver) Kill() error {
	return d.OperateVM("kill")
}

// Remove removes the VM
func (d *Driver) Remove() error {
	vm, err := d.GetVM()
	if err != nil {
		return err
	}

	stopTask, err2 := vm.Stop(context.Background())
	if err2 != nil {
		return err2
	}
	// wait for the stop task
	if err3 := stopTask.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err3 != nil {
		return err3
	}

	deleteTask, err4 := vm.Delete(context.Background())
	if err4 != nil {
		return err4
	}

	// wait for the delete task
	if err5 := deleteTask.Wait(context.Background(), time.Duration(5*time.Second), time.Duration(300*time.Second)); err5 != nil {
		return err5
	}

	return nil
}

func (d *Driver) GetVmidInRange() (int, error) {
	// split d.VMIDRange into two parts by separating through ":"
	vmidRange := strings.Split(d.VMIDRange, ":")
	if len(vmidRange) != 2 {
		return 0, fmt.Errorf("VMIDRange must be in the form of <min>:<max>. Given: %s", d.VMIDRange)
	}

	min, err := strconv.Atoi(vmidRange[0])
	if err != nil {
		return 0, err
	}

	max, err := strconv.Atoi(vmidRange[1])
	if err != nil {
		return 0, err
	}

	if min > max {
		return 0, fmt.Errorf("VMIDRange :<max> must be greater than <min>. Given: %s", d.VMIDRange)
	}
	// now randomly choose from min max range
	return rand.Intn(max-min) + min, nil
}

func (d *Driver) createSSHKey() (string, error) {
	var sshKeyPath = d.GetSSHKeyPath()
	d.debugf("Creating SSH key at %s", sshKeyPath)

	if err := ssh.GenerateSSHKey(sshKeyPath); err != nil {
		return "", err
	}

	key, err := os.ReadFile(sshKeyPath + ".pub")
	d.debugf("Read SSH key from %s: %s", sshKeyPath, key)
	if err != nil {
		return "", err
	}
	return string(key), nil
}
