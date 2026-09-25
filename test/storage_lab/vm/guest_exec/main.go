// guest_exec inspects an existing lab through its native SDK control plane.
// It never provisions, restarts, or closes the target session.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
)

func main() {
	os.Exit(run())
}

func run() int {
	socket := flag.String("socket", "", "existing labd socket")
	session := flag.String("session", "", "existing lab session ID")
	node := flag.String("node", "vm", "guest node")
	timeout := flag.Duration("timeout", 30*time.Second, "bounded guest command timeout")
	flag.Parse()
	if *socket == "" || *session == "" || len(flag.Args()) == 0 {
		flag.Usage()
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, *socket)
	if err != nil {
		panic(err)
	}
	defer c.Close()
	r, err := c.RPC().Exec(ctx, &labv1.ExecRequest{Node: &labv1.NodeRef{SessionId: *session, Node: *node}, Argv: flag.Args(), TimeoutMillis: timeout.Milliseconds()})
	if err != nil {
		panic(err)
	}
	fmt.Print(string(r.GetStdout()))
	fmt.Fprint(os.Stderr, string(r.GetStderr()))
	return int(r.GetExitCode())
}
