package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

type snapshotView struct {
	Name      string    `json:"name"`
	Arch      string    `json:"arch"`
	CPUs      int       `json:"cpus"`
	MemMB     int       `json:"mem_mb"`
	Accel     string    `json:"accel"`
	CreatedAt time.Time `json:"created_at"`
	Consumed  bool      `json:"consumed"`
}

func CmdSnapshot(args []string) int {
	if len(args) == 0 {
		return fatalf("usage: pvm snapshot create <name> | ls [name] | rm <name>")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fatalf("usage: pvm snapshot create <name>")
		}
		var man snapshotView
		if err := c.Post("/v1/vms/"+args[1]+"/snapshot", nil, &man); err != nil {
			return fatalf("snapshot: %v", err)
		}
		fmt.Printf("snapshot of %s saved (%s/%dc/%dMB, %s); vm is stopped, `pvm start %s` restores it\n",
			man.Name, man.Arch, man.CPUs, man.MemMB, man.CreatedAt.Format("2006-01-02 15:04:05"), man.Name)
		return 0
	case "ls", "list":
		var infos []vmInfoView
		if err := c.Get("/v1/vms", &infos); err != nil {
			return fatalf("%v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "VM\tCREATED\tCONFIG\tCONSUMED")
		n := 0
		for _, in := range infos {
			if len(args) > 1 && in.Spec.Name != args[1] {
				continue
			}
			var man snapshotView
			if err := c.Get("/v1/vms/"+in.Spec.Name+"/snapshot", &man); err != nil {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s/%dc/%dMB %s\t%v\n",
				man.Name, man.CreatedAt.Format("2006-01-02 15:04:05"),
				man.Arch, man.CPUs, man.MemMB, man.Accel, man.Consumed)
			n++
		}
		tw.Flush()
		if n == 0 {
			fmt.Println("no snapshots")
		}
		return 0
	case "rm":
		if len(args) < 2 {
			return fatalf("usage: pvm snapshot rm <name>")
		}
		if err := c.Delete("/v1/vms/"+args[1]+"/snapshot", nil); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("snapshot of %s removed\n", args[1])
		return 0
	}
	return fatalf("unknown snapshot subcommand %q", args[0])
}

// CmdSnapshotPipeOut/In are hidden helpers invoked by QEMU exec:
// migration URIs; not user-facing commands.
func CmdSnapshotPipeOut(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pvm __snapshot-pipe-out <file>")
		return 2
	}
	if err := qemu.SnapshotPipeOut(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "pipe-out: %v\n", err)
		return 1
	}
	return 0
}

func CmdSnapshotPipeIn(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pvm __snapshot-pipe-in <file>")
		return 2
	}
	if err := qemu.SnapshotPipeIn(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "pipe-in: %v\n", err)
		return 1
	}
	return 0
}
