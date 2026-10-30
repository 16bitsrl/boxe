package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A fork is a copy of a running VM with its memory: programs keep running
// in the copy where they were. The source is paused while its disk and
// memory are saved together, about a second; the copy is restored from them
// like a VM waking up (sleep.go), then boxe-agent in the copy swaps in its
// own name, keys, MAC and IP address. Until then the network drops whatever
// the copy sends with its source's addresses (net-setup.sh).

// agentPort is where boxe-agent listens in the VM (image/boxe-agent).
const agentPort = 1024

// agent sends cmd to boxe-agent in the VM and returns an error unless it
// answers ok.
func (h *Hypervisor) agent(name, cmd string, timeout time.Duration) error {
	_, err := h.ask(name, cmd, timeout)
	return err
}

// ask sends cmd to boxe-agent in the VM and returns what it sends after its
// ok, until it hangs up; an error unless it answers ok. The VM's vsock is a
// Unix socket on the host: Cloud Hypervisor connects us to a port in the VM
// after a CONNECT line.
func (h *Hypervisor) ask(name, cmd string, timeout time.Duration) (string, error) {
	c, err := h.dial("unix", vsockPath(name), 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errAgentUnreachable, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	r := bufio.NewReader(c)
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", agentPort); err != nil {
		return "", err
	}
	if line, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(line, "OK ") {
		return "", fmt.Errorf("%w: %q %v", errAgentUnreachable, line, err)
	}
	if _, err := fmt.Fprintf(c, "%s\n", cmd); err != nil {
		return "", err
	}
	reply, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("%w: %v", errAgentHungUp, err)
	}
	if reply = strings.TrimSpace(reply); reply != "ok" {
		return "", fmt.Errorf("boxe-agent: %s", reply)
	}
	// What the VM sends is capped: it is the VM's to make up.
	rest, err := io.ReadAll(io.LimitReader(r, 1<<20))
	return string(rest), err
}

var (
	errNoAgent          = errors.New("no boxe-agent")
	errAgentHungUp      = errors.New("boxe-agent hung up")
	errAgentUnreachable = errors.New("boxe-agent not reachable")
)

// Fork creates v as a copy of the running VM src, memory included. On
// failure it cleans up; if that fails too the error wraps errLeftBehind.
func (h *Hypervisor) Fork(src, v VM, authorizedKeys string) error {
	if h.SleepDir == "" {
		return errors.New("forking is off on this host")
	}
	if !h.Running(src.Name) {
		return fmt.Errorf("%s is not running", src.Name)
	}
	if err := h.agent(src.Name, "ping", 3*time.Second); err != nil {
		return fmt.Errorf("%w: %s comes from an older image and cannot be forked: copy it with cp (%v)", errNoAgent, src.Name, err)
	}
	snap, err := h.saveRunning(src, v.Name)
	if err != nil {
		return err
	}
	err = h.forkFrom(src, v, snap, authorizedKeys)
	if err == nil {
		return nil
	}
	h.forgetSleep(src.Name)
	if derr := h.Destroy(v); derr != nil {
		return fmt.Errorf("%w: %v (cleaning up: %v)", errLeftBehind, err, derr)
	}
	h.run("zfs", "destroy", snap)
	return err
}

// saveRunning saves src's disk (a ZFS snapshot, returned) and memory (in
// its sleep directory) at the same moment, and lets it run again.
func (h *Hypervisor) saveRunning(src VM, copyName string) (string, error) {
	if err := h.forgetSleep(src.Name); err != nil {
		return "", err
	}
	if _, err := h.chRemote(src.Name, "pause"); err != nil {
		return "", err
	}
	snap := fmt.Sprintf("%s@cp-%s-%d", h.ds(src.Name), copyName, time.Now().Unix())
	err := h.run("zfs", "snapshot", snap)
	if err == nil {
		if _, err = h.chRemote(src.Name, "snapshot", "file://"+h.sleepDir(src.Name)); err != nil {
			h.run("zfs", "destroy", snap)
		}
	}
	if _, rerr := h.chRemote(src.Name, "resume"); rerr != nil && err == nil {
		err = fmt.Errorf("%s did not resume: %v", src.Name, rerr)
	}
	if err != nil {
		h.forgetSleep(src.Name)
		return "", err
	}
	return snap, nil
}

func (h *Hypervisor) forkFrom(src, v VM, snap, authorizedKeys string) error {
	if err := h.run("zfs", "clone", snap, h.ds(v.Name)); err != nil {
		return err
	}
	if err := h.writeConfig(v, authorizedKeys); err != nil {
		return err
	}
	if err := h.waitDev(v); err != nil {
		return err
	}
	// The saved memory becomes the copy's, as if it had gone to sleep.
	dir := h.sleepDir(v.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if h.VMUserID > 0 {
		if err := os.Chown(dir, h.VMUserID, h.VMGroupID); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(h.sleepDir(src.Name))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(h.sleepDir(src.Name), e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, sleepDone), nil, 0o600); err != nil {
		return err
	}
	if err := h.Wake(v); err != nil {
		return err
	}
	// A fresh hypervisor numbers its connections from the same port as the
	// source's did. If the source had just been started, the ping in Fork
	// may still be closing in the saved memory: when it closes, it cuts off
	// the copy's connection on that port, after the command got through.
	// So pings first, until one gets through on another port.
	for i := 0; ; i++ {
		err := h.agent(v.Name, "ping", time.Second)
		if err == nil {
			break
		}
		if !errors.Is(err, errAgentHungUp) || i == 2 {
			return err
		}
	}
	return h.agent(v.Name, "reconfigure", 30*time.Second)
}
