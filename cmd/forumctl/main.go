package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"webfart/internal/api"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	switch command {
	case "tombstone":
		cmdTombstone(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Usage: forumctl <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  tombstone  -t <thread> -s <seq> [-sock <socket_path>]")
}

func cmdTombstone(args []string) {
	fs := flag.NewFlagSet("tombstone", flag.ExitOnError)
	threadFlag := fs.String("t", "", "Thread identifier (single character a-z)")
	seqFlag := fs.Uint64("s", 0, "Message sequence number to tombstone")
	sockFlag := fs.String("sock", "/run/forum/admin.sock", "Path to admin unix domain socket")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}

	if *threadFlag == "" || *seqFlag == 0 {
		fmt.Fprintln(os.Stderr, "Both -t <thread> and -s <seq> are required.")
		fs.Usage()
		os.Exit(1)
	}

	// Try default socket path; if not found, check ./threads/admin.sock or ./admin.sock
	sockPath := *sockFlag
	if _, err := os.Stat(sockPath); os.IsNotExist(err) {
		if _, err := os.Stat("./threads/admin.sock"); err == nil {
			sockPath = "./threads/admin.sock"
		} else if _, err := os.Stat("./admin.sock"); err == nil {
			sockPath = "./admin.sock"
		}
	}

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to admin socket %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer conn.Close()

	cmd := api.AdminCommand{
		Action: "tombstone",
		Thread: *threadFlag,
		Seq:    *seqFlag,
	}

	cmdBytes, _ := json.Marshal(cmd)
	if _, err := conn.Write(append(cmdBytes, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write command: %v\n", err)
		os.Exit(1)
	}

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "Empty response from admin socket")
		os.Exit(1)
	}

	var resp api.AdminResponse
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		fmt.Fprintf(os.Stderr, "Malformed response: %s\n", scanner.Text())
		os.Exit(1)
	}

	if !resp.OK {
		fmt.Fprintf(os.Stderr, "Tombstone failed: %s\n", resp.Error)
		os.Exit(1)
	}

	fmt.Printf("Successfully tombstoned message %d in thread /%s/\n", *seqFlag, *threadFlag)
}
