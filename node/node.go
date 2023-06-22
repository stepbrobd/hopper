package node

import (
	"bytes"
	"fmt"
	"log"
	"net/rpc"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"

	c "github.com/Cybergenik/hopper/common"
)

type HopperNode struct {
	outDir string
	id     uint64
	target string
	args   string
	env    []string
	stdin  bool
	raw    bool
	master string
	crashN uint64
	dead   int32
	conn   *rpc.Client
}

func (n *HopperNode) getFTask() (c.FTask, bool) {
	args := c.FTaskArgs{}
	t := c.FTask{}

	if ok := n.call("Hopper.GetFTask", &args, &t); !ok {
		log.Println("Error Getting FTask!")
		return t, ok
	}
	return t, true
}

// Returns Bool : True if Master wants to Log crash
func (n *HopperNode) updateFTask(ut c.UpdateFTask) bool {
	reply := c.UpdateReply{}
	if ok := n.call("Hopper.UpdateFTask", &ut, &reply); !ok {
		log.Println("Error Updating FTask!")
	}
	return reply.Log
}

func (n *HopperNode) killed() bool {
	z := atomic.LoadInt32(&n.dead)
	return z == 1
}

func (n *HopperNode) fuzz(t c.FTask) {
	//Run seed
	var fuzzCommand []string
	var seed string
	// Should seed be passed in as a file or raw
	if n.raw {
		seed = string(t.Seed)
	} else if !n.stdin {
		f, err := os.CreateTemp("", "hopper.*.in")
		defer os.Remove(f.Name())
		if err != nil {
			log.Fatal(err)
		}
		if _, err := f.Write(t.Seed); err != nil {
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
		seed = f.Name()
	}
	// Stdin or file
	if n.stdin {
		fuzzCommand = strings.Split(n.args, " ")
	} else {
		fuzzCommand = strings.Split(
			strings.Replace(
				n.args,
				"@@",
				seed,
				1,
			),
			" ",
		)
	}
	update := c.UpdateFTask{
		NodeId: n.id,
		Id:     t.Id,
	}
	cmd := exec.Command(n.target, fuzzCommand...)
	cmd.Env = append(os.Environ(), n.env...)
	// Gather err output
	var errOut bytes.Buffer
	var stdin bytes.Buffer
	cmd.Stderr = &errOut
	if n.stdin {
		cmd.Stdin = &stdin
	}
	if err := cmd.Start(); err != nil {
		log.Println(err)
		update.Ok = false
		go n.updateFTask(update)
		return
	}
	if n.stdin {
		stdin.Write(t.Seed)
	}
	err := cmd.Wait()
	sancov_file := fmt.Sprintf("%s.%v.sancov",
		filepath.Base(n.target),
		cmd.Process.Pid,
	)
	//Crash Detected
	if err != nil {
		if report := ParseAsan(errOut.String()); report != "" {
			update.CrashMsg = report
			update.Crash = true
			n.crashN++
		}
	}
	//Generate Coverage data
	cov_s, ok := GetCoverage(sancov_file)
	update.Ok = ok
	go func(N uint64) {
		if update.Ok {
			update.CovEdges = uint64(len(cov_s))
			update.CovHash = c.BloomHash([]byte(strings.Join(cov_s, "")))
		}
		log := n.updateFTask(update)
		if log {
			PersistCrash(t.Seed, errOut, N, n.outDir)
		}
	}(n.crashN)
}

func (n *HopperNode) taskGenerator(taskQ chan c.FTask) {
	for !n.killed() {
		ftask, ok := n.getFTask()
		if !ok || ftask.Die {
			atomic.StoreInt32(&n.dead, 1)
			return
		}
		taskQ <- ftask
	}
}

func Node(id uint64, target string, args string, raw bool, env string, stdin bool, master string) {
	out_dir := os.Getenv("HOPPER_OUT")
	var location string
	if out_dir != "" {
		location = path.Join(out_dir, fmt.Sprintf("Node%d", id))
	} else {
		location = fmt.Sprintf("Node%d", id)
	}
	n := HopperNode{
		outDir: location,
		id:     id,
		target: target,
		args:   args,
		env:    strings.Split(env, ";"),
		stdin:  stdin,
		raw:    raw,
		master: master,
		crashN: 0,
	}

	// Check target executable exists
	if _, err := os.Stat(target); err != nil {
		log.Fatal(err)
	}
	// Env vars
	n.env = append(n.env, "ASAN_OPTIONS=coverage=1")
	// Init TCP/IP connection to master
	conn, err := rpc.DialHTTP("tcp", n.master)
	if err != nil {
		log.Fatal("dialing:", err)
	}
	n.conn = conn
	defer n.conn.Close()
	// Create node out dir
	if err := os.MkdirAll(n.outDir, 0750); err != nil && !os.IsExist(err) {
		log.Fatal(err)
	}
	//Double Thread Queue: request Task -> do Task
	taskQ := make(chan c.FTask, 5)
	go n.taskGenerator(taskQ)

	log.Printf("Started Node: %v\n", id)
	for !n.killed() {
		select {
		case task := <-taskQ:
			n.fuzz(task)
		default:
		}
	}
}

func (n *HopperNode) call(rpcname string, args interface{}, reply interface{}) bool {
	err := n.conn.Call(rpcname, args, reply)
	if err != nil {
		log.Print(err)
		return false
	}

	return true
}
