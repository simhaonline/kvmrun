package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/0xef53/kvmrun/pkg/block"
	"github.com/0xef53/kvmrun/pkg/kvmrun"
	"github.com/0xef53/kvmrun/pkg/lvm"
	"github.com/0xef53/kvmrun/pkg/rpc/client"
	"github.com/0xef53/kvmrun/pkg/rpc/common"

	"github.com/0xef53/cli"

	"github.com/gosuri/uiprogress"
	"github.com/gosuri/uiprogress/util/strutil"
)

var cmdCopyConfig = cli.Command{
	Name:      "copy-config",
	Usage:     "copy virtual machine configuration to another host",
	ArgsUsage: "VMNAME DSTSERVER",
	Flags: []cli.Flag{
		cli.GenericFlag{Name: "override-disk", PlaceHolder: "disk1:disk2", Value: NewStringMap(), Usage: "override disk path/name on the destination server"},
	},
	Action: func(c *cli.Context) {
		os.Exit(executeRPC(c, copyConfig))
	},
}

func copyConfig(vmname string, live bool, c *cli.Context, client *rpcclient.UnixClient) (errors []error) {
	dstServer := c.Args().Tail()[0]
	overriddenDisks := c.Generic("override-disk").(*StringMap).Value()

	req := rpccommon.InstanceRequest{
		Name: vmname,
		Data: &rpccommon.MigrationParams{
			DstServer: dstServer,
			Overrides: rpccommon.MigrationOverrides{
				Disks: overriddenDisks,
			},
		},
	}

	if err := client.Request("RPC.CopyConfig", &req, nil); err != nil {
		return append(errors, err)
	}

	return errors
}

var cmdMigrate = cli.Command{
	Name:      "migrate",
	Usage:     "migrate the virtual machine to another host",
	ArgsUsage: "VMNAME DSTSERVER",
	Flags: []cli.Flag{
		cli.BoolFlag{Name: "watch,w", Usage: "watch the migration process"},
		cli.BoolFlag{Name: "with-local-disks", Usage: "enable live storage migration for all local disks (conflicts with option --with-disk)"},
		cli.StringSliceFlag{Name: "with-disk", Usage: "enable live storage migration for specified disks (conflicts with option --with-local-disks)"},
		cli.GenericFlag{Name: "override-disk", PlaceHolder: "disk1:disk2", Value: NewStringMap(), Usage: "override disk path/name on the destination server"},
		cli.BoolFlag{Name: "create-lv", Usage: "create logical volumes in the same group on the destination server"},
	},
	Action: func(c *cli.Context) {
		os.Exit(executeRPC(c, migrate))
	},
}

func migrate(vmname string, live bool, c *cli.Context, client *rpcclient.UnixClient) (errors []error) {
	dstServer := c.Args().Tail()[0]
	withLocalDisks := c.Bool("with-local-disks")
	chosenDisks := c.StringSlice("with-disk")
	overriddenDisks := c.Generic("override-disk").(*StringMap).Value()

	jsonReq := rpccommon.InstanceRequest{
		Name: vmname,
	}

	var jsonResp []byte

	if err := client.Request("RPC.GetInstanceJSON", &jsonReq, &jsonResp); err != nil {
		return append(errors, err)
	}

	vm := struct {
		R *kvmrun.InstanceQemu `json:"run"`
	}{}
	if err := json.Unmarshal(jsonResp, &vm); err != nil {
		return append(errors, err)
	}

	// Only running virtual machines can be migrated
	if vm.R == nil {
		return append(errors, &kvmrun.NotRunningError{vmname})
	}

	attachedDisks := vm.R.GetDisks()

	disksToMigrate := make([]string, 0, len(attachedDisks))

	switch {
	case len(chosenDisks) > 0:
		for _, p := range chosenDisks {
			if attachedDisks.Exists(p) {
				disksToMigrate = append(disksToMigrate, p)
			} else {
				return append(errors, fmt.Errorf("Unable to migrate unknown disk: %s", p))
			}
		}
	case withLocalDisks:
		for _, d := range attachedDisks {
			b, err := kvmrun.NewDiskBackend(d.Path)
			if err != nil {
				return append(errors, err)
			}
			if b.IsLocal() {
				disksToMigrate = append(disksToMigrate, d.Path)
			}
		}
	}

	// This is a temporary solution.
	// TODO: It should accept different types of disks (LVM/QCOW)
	// and pass them to a destination server.
	createDisksOnDst := func() error {
		lvmDisks := make(map[string]uint64)
		if len(disksToMigrate) > 0 {
			for _, d := range disksToMigrate {
				switch ok, err := lvm.IsLogicalVolume(d); {
				case ok:
				case !ok, err == nil:
					return fmt.Errorf("Not a logical volume: %s", d)
				default:
					return err
				}
				s, err := block.BlkGetSize64(d)
				if err != nil {
					return err
				}
				if v, ok := overriddenDisks[d]; ok {
					lvmDisks[v] = s
				} else {
					lvmDisks[d] = s
				}
			}
		}

		if len(lvmDisks) > 0 {
			req := rpccommon.CreateDisksRequest{
				Disks:     lvmDisks,
				DstServer: dstServer,
			}
			if err := client.Request("RPC.PrepareDstDisks", &req, nil); err != nil {
				return err
			}
		}

		return nil
	}

	if c.Bool("create-lv") {
		if err := createDisksOnDst(); err != nil {
			return append(errors, err)
		}
	}

	migrReq := rpccommon.InstanceRequest{
		Name: vmname,
		Data: &rpccommon.MigrationParams{
			DstServer: dstServer,
			Disks:     disksToMigrate,
			Overrides: rpccommon.MigrationOverrides{
				Disks: overriddenDisks,
			},
		},
	}

	if err := client.Request("RPC.StartMigrationProcess", &migrReq, nil); err != nil {
		return append(errors, err)
	}

	if c.Bool("watch") {
		return migrateStatus(vmname, live, c, client)
	} else {
		fmt.Println("Migration started")
		fmt.Println("Note: command 'migrate-status' shows the migration progress")
	}

	return errors
}

var cmdMigrateCancel = cli.Command{
	Name:      "migrate-cancel",
	Usage:     "cancel a running migration process",
	ArgsUsage: "VMNAME",
	Action: func(c *cli.Context) {
		os.Exit(executeRPC(c, migrateCancel))
	},
}

func migrateCancel(vmname string, live bool, c *cli.Context, client *rpcclient.UnixClient) (errors []error) {
	req := rpccommon.InstanceRequest{
		Name: vmname,
	}

	if err := client.Request("RPC.CancelMigrationProcess", &req, nil); err != nil {
		return append(errors, err)
	}

	fmt.Println("OK, cancelled")

	return errors
}

var cmdMigrateStatus = cli.Command{
	Name:      "migrate-status",
	Usage:     "check migration's progress or a final result",
	ArgsUsage: "VMNAME",
	Action: func(c *cli.Context) {
		os.Exit(executeRPC(c, migrateStatus))
	},
}

func migrateStatus(vmname string, live bool, c *cli.Context, client *rpcclient.UnixClient) (errors []error) {
	req := rpccommon.VMNameRequest{
		Name: vmname,
	}

	st := rpccommon.MigrationStat{}

	if err := client.Request("RPC.GetMigrationStat", &req, &st); err != nil {
		return append(errors, err)
	}

	// Just print and exit
	if c.GlobalBool("json") {
		jB, err := json.MarshalIndent(st, "", "    ")
		if err != nil {
			return append(errors, err)
		}

		fmt.Printf("%s\n", string(jB))

		return errors
	}

	switch st.Status {
	case "completed":
		fmt.Println("Successfully migrated to", st.DstServer)
		return errors
	case "none":
		return append(errors, fmt.Errorf("Migration is not running"))
	case "failed":
		return append(errors, fmt.Errorf("Migration failed: %s", st.Desc))
	case "interrupted":
		return append(errors, fmt.Errorf("Migration is interrupted"))
	}

	completed := make(chan struct{})
	barPipes := make(map[string]chan *rpccommon.StatInfo, 10)

	// This function prints a progress bar for each disk and a qemu vmstate
	process := func(name string, pipe <-chan *rpccommon.StatInfo) {
		bar := uiprogress.AddBar(100).AppendCompleted()
		bar.Width = 50

		var status string

		bar.PrependFunc(func(b *uiprogress.Bar) string {
			return strutil.Resize(fmt.Sprintf("%s: %*s", name, (32-len(name)), status), 35)
		})

		for {
			select {
			case <-completed:
				bar.Set(100)
				return
			case x := <-pipe:
				switch {
				case x.Percent == 0:
					status = "waiting"
				case x.Percent == 100:
					status = "completed"
				default:
					status = "syncing"
				}
				bar.Set(int(x.Percent))
			}
		}
	}

	uiprogress.Start()

	for diskpath := range st.Disks {
		barPipes[diskpath] = make(chan *rpccommon.StatInfo)
		go process(diskpath, barPipes[diskpath])
		barPipes[diskpath] <- st.Disks[diskpath]
	}

	barPipes["qemu_vmstate"] = make(chan *rpccommon.StatInfo)
	go process(vmname, barPipes["qemu_vmstate"])
	barPipes["qemu_vmstate"] <- st.Qemu

	// Watch the progress ...
loop:
	for {
		st := rpccommon.MigrationStat{}

		if err := client.Request("RPC.GetMigrationStat", &req, &st); err != nil {
			return append(errors, err)
		}

		barPipes["qemu_vmstate"] <- st.Qemu

		for diskpath := range st.Disks {
			barPipes[diskpath] <- st.Disks[diskpath]
		}

		time.Sleep(1 * time.Second)

		switch st.Status {
		case "completed":
			close(completed)
			break loop
		case "failed", "interrupted", "none":
			break loop
		}
	}

	uiprogress.Stop()
	fmt.Println()

	// Print results
	if err := client.Request("RPC.GetMigrationStat", &req, &st); err != nil {
		return append(errors, err)
	}

	switch st.Status {
	case "completed":
		fmt.Println("Successfully migrated to", st.DstServer)
	case "failed":
		errors = append(errors, fmt.Errorf("Migration failed: %s", st.Desc))
	case "interrupted", "none":
		errors = append(errors, fmt.Errorf("Migration is interrupted"))
	}

	return errors
}
