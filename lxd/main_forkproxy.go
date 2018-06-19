package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/spf13/cobra"

	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/eagain"
)

/*
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/epoll.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

extern char* advance_arg(bool required);
extern void attach_userns(int pid);
extern int dosetns(int pid, char *nstype);

int whoami = -ESRCH;

#define FORKPROXY_CHILD 1
#define FORKPROXY_PARENT 0
#define FORKPROXY_UDS_SOCK_FD_NUM 200

int wait_for_pid(pid_t pid)
{
	int status, ret;

again:
	ret = waitpid(pid, &status, 0);
	if (ret == -1) {
		if (errno == EINTR)
			goto again;
		return -1;
	}
	if (ret != pid)
		goto again;
	if (!WIFEXITED(status) || WEXITSTATUS(status) != 0)
		return -1;
	return 0;
}

ssize_t lxc_read_nointr(int fd, void* buf, size_t count)
{
	ssize_t ret;
again:
	ret = read(fd, buf, count);
	if (ret < 0 && errno == EINTR)
		goto again;
	return ret;
}

void forkproxy()
{
	int connect_pid, listen_pid, log_fd;
	ssize_t ret;
	pid_t pid;
	char *connect_addr, *cur, *listen_addr, *log_path, *pid_path;
	int sk_fds[2] = {-EBADF, -EBADF};
	FILE *pid_file;

	// Get the pid
	cur = advance_arg(false);
	if (cur == NULL ||
	    (strcmp(cur, "--help") == 0 || strcmp(cur, "--version") == 0 ||
	     strcmp(cur, "-h") == 0))
		_exit(EXIT_FAILURE);

	listen_pid = atoi(cur);
	listen_addr = advance_arg(true);
	connect_pid = atoi(advance_arg(true));
	connect_addr = advance_arg(true);
	log_path = advance_arg(true);
	pid_path = advance_arg(true);

	close(STDIN_FILENO);
	log_fd = open(log_path, O_WRONLY | O_CREAT | O_CLOEXEC | O_TRUNC, 0600);
	if (log_fd < 0)
		_exit(EXIT_FAILURE);

	ret = dup3(log_fd, STDOUT_FILENO, O_CLOEXEC);
	if (ret < 0)
		_exit(EXIT_FAILURE);

	ret = dup3(log_fd, STDERR_FILENO, O_CLOEXEC);
	if (ret < 0)
		_exit(EXIT_FAILURE);

	pid_file = fopen(pid_path, "w+");
	if (!pid_file) {
		fprintf(stderr,
			"%s - Failed to create pid file for proxy daemon\n",
			strerror(errno));
		_exit(EXIT_FAILURE);
	}

	if (strncmp(listen_addr, "udp:", sizeof("udp:") - 1) == 0 &&
	    strncmp(connect_addr, "udp:", sizeof("udp:") - 1) != 0) {
		    fprintf(stderr, "Error: Proxying from udp to non-udp "
			    "protocol is not supported\n");
		    _exit(EXIT_FAILURE);
	}

	ret = socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, sk_fds);
	if (ret < 0) {
		fprintf(stderr,
			"%s - Failed to create anonymous unix socket pair\n",
			strerror(errno));
		_exit(EXIT_FAILURE);
	}
	fprintf(stderr, "Created anonymous pair {%d,%d} of unix sockets\n",
		sk_fds[0], sk_fds[1]);

	pid = fork();
	if (pid < 0) {
		fprintf(stderr, "%s - Failed to create new process\n",
			strerror(errno));
		_exit(EXIT_FAILURE);
	}

	if (pid == 0) {
		whoami = FORKPROXY_CHILD;

		fclose(pid_file);
		close(sk_fds[0]);

		// Attach to the user namespace of the listener
		attach_userns(listen_pid);

		// Attach to the network namespace of the listener
		ret = dosetns(listen_pid, "net");
		if (ret < 0) {
			fprintf(stderr, "Failed setns to listener network namespace: %s\n",
				strerror(errno));
			_exit(EXIT_FAILURE);
		}

		// Attach to the mount namespace of the listener
		ret = dosetns(listen_pid, "mnt");
		if (ret < 0) {
			fprintf(stderr, "Failed setns to listener mount namespace: %s\n",
				strerror(errno));
			_exit(EXIT_FAILURE);
		}

		ret = dup3(sk_fds[1], FORKPROXY_UDS_SOCK_FD_NUM, O_CLOEXEC);
		if (ret < 0) {
			fprintf(stderr,
				"%s - Failed to duplicate fd %d to fd 200\n",
				strerror(errno), sk_fds[1]);
			_exit(1);
		}

		ret = close(sk_fds[1]);
		if (ret < 0) {
			fprintf(stderr, "%s - Failed to close socket fd %d\n",
				strerror(errno), sk_fds[1]);
			_exit(1);
		}
	} else {
		whoami = FORKPROXY_PARENT;

		close(sk_fds[1]);

		// Attach to the user namespace of the listener
		attach_userns(connect_pid);

		// Attach to the network namespace of the listener
		ret = dosetns(connect_pid, "net");
		if (ret < 0) {
			fprintf(stderr, "Failed setns to listener network namespace: %s\n",
				strerror(errno));
			_exit(EXIT_FAILURE);
		}

		// Attach to the mount namespace of the listener
		ret = dosetns(connect_pid, "mnt");
		if (ret < 0) {
			fprintf(stderr, "Failed setns to listener mount namespace: %s\n",
				strerror(errno));
			_exit(EXIT_FAILURE);
		}

		ret = dup3(sk_fds[0], FORKPROXY_UDS_SOCK_FD_NUM, O_CLOEXEC);
		if (ret < 0) {
			fprintf(stderr,
				"%s - Failed to duplicate fd %d to fd 200\n",
				strerror(errno), sk_fds[1]);
			_exit(1);
		}

		ret = close(sk_fds[0]);
		if (ret < 0) {
			fprintf(stderr, "%s - Failed to close socket fd %d\n",
				strerror(errno), sk_fds[1]);
			_exit(1);
		}

		ret = wait_for_pid(pid);
		if (ret < 0) {
			fprintf(stderr, "Failed to start listener\n");
			_exit(EXIT_FAILURE);
		}

		// daemonize
		pid = fork();
		if (pid < 0)
			_exit(EXIT_FAILURE);

		if (pid != 0) {
			ret = wait_for_pid(pid);
			if (ret < 0)
				_exit(EXIT_FAILURE);

			_exit(EXIT_SUCCESS);
		}

		pid = fork();
		if (pid < 0)
			_exit(EXIT_FAILURE);

		if (pid != 0) {
			ret = fprintf(pid_file, "%d", pid);
			fclose(pid_file);
			if (ret < 0) {
				fprintf(stderr, "Failed to write proxy daemon pid %d to \"%s\"\n",
					pid, pid_path);
				ret = EXIT_FAILURE;
			}
			close(STDOUT_FILENO);
			close(STDERR_FILENO);
			_exit(EXIT_SUCCESS);
		}

		ret = setsid();
		if (ret < 0)
			fprintf(stderr, "%s - Failed to setsid in proxy daemon\n",
				strerror(errno));
	}
}
*/
import "C"

const forkproxyUDSSockFDNum int = C.FORKPROXY_UDS_SOCK_FD_NUM

type cmdForkproxy struct {
	global *cmdGlobal
}

type proxyAddress struct {
	connType string
	addr     []string
	abstract bool
}

func (c *cmdForkproxy) Command() *cobra.Command {
	// Main subcommand
	cmd := &cobra.Command{}
	cmd.Use = "forkproxy <listen PID> <listen address> <connect PID> <connect address> <fd> <reexec> <log path> <pid path>"
	cmd.Short = "Setup network connection proxying"
	cmd.Long = `Description:
  Setup network connection proxying

  This internal command will spawn a new proxy process for a particular
  container, connecting one side to the host and the other to the
  container.
`
	cmd.RunE = c.Run
	cmd.Hidden = true

	return cmd
}

func rearmUDPFd(epFd C.int, connFd C.int) {
	var ev C.struct_epoll_event
	ev.events = C.EPOLLIN | C.EPOLLONESHOT
	*(*C.int)(unsafe.Pointer(uintptr(unsafe.Pointer(&ev)) + unsafe.Sizeof(ev.events))) = connFd
	ret := C.epoll_ctl(epFd, C.EPOLL_CTL_MOD, connFd, &ev)
	if ret < 0 {
		fmt.Printf("Error: Failed to add listener fd to epoll instance\n")
	}
}

func listenerInstance(epFd C.int, lAddr *proxyAddress, cAddr *proxyAddress, connFd C.int, lStruct *lStruct) error {
	fmt.Printf("Starting %s <-> %s proxy\n", lAddr.connType, cAddr.connType)
	if lAddr.connType == "udp" {
		// This only handles udp <-> udp. The C constructor will have
		// verified this before.
		go func() {
			// single or multiple port -> single port
			connectAddr := cAddr.addr[0]
			if len(cAddr.addr) > 1 {
				// multiple port -> multiple port
				connectAddr = cAddr.addr[(*lStruct).lAddrIndex]
			}

			srcConn, err := net.FileConn((*lStruct).f)
			if err != nil {
				fmt.Printf("Failed to re-assemble listener: %s", err)
				rearmUDPFd(epFd, connFd)
				return
			}

			dstConn, err := net.Dial(cAddr.connType, connectAddr)
			if err != nil {
				fmt.Printf("Error: Failed to connect to target: %v\n", err)
				rearmUDPFd(epFd, connFd)
				return
			}

			genericRelay(srcConn, dstConn)
			rearmUDPFd(epFd, connFd)
		}()

		return nil
	}

	// Accept a new client
	listener := (*lStruct).lConn
	srcConn, err := (*listener).Accept()
	if err != nil {
		fmt.Printf("Error: Failed to accept new connection: %v\n", err)
		return err
	}

	// single or multiple port -> single port
	connectAddr := cAddr.addr[0]
	if lAddr.connType != "unix" && cAddr.connType != "unix" && len(cAddr.addr) > 1 {
		// multiple port -> multiple port
		connectAddr = cAddr.addr[(*lStruct).lAddrIndex]
	}
	dstConn, err := net.Dial(cAddr.connType, connectAddr)
	if err != nil {
		srcConn.Close()
		fmt.Printf("Error: Failed to connect to target: %v\n", err)
		return err
	}

	if cAddr.connType == "unix" && lAddr.connType == "unix" {
		// Handle OOB if both src and dst are using unix sockets
		go unixRelay(srcConn, dstConn)
	} else {
		go genericRelay(srcConn, dstConn)
	}

	return nil
}

type lStruct struct {
	f          *os.File
	lConn      *net.Listener
	udpConn    *net.Conn
	lAddrIndex int
}

func (c *cmdForkproxy) Run(cmd *cobra.Command, args []string) error {
	// Only root should run this
	if os.Geteuid() != 0 {
		return fmt.Errorf("This must be run as root")
	}

	// Sanity checks
	if len(args) != 6 {
		cmd.Help()

		if len(args) == 0 {
			return nil
		}

		return fmt.Errorf("Missing required arguments")
	}

	// Check where we are in initialization
	if C.whoami != C.FORKPROXY_PARENT && C.whoami != C.FORKPROXY_CHILD {
		return fmt.Errorf("Failed to call forkproxy constructor")
	}

	listenAddr := args[1]
	lAddr, err := parseAddr(listenAddr)
	if err != nil {
		return err
	}

	connectAddr := args[3]
	cAddr, err := parseAddr(connectAddr)
	if err != nil {
		return err
	}

	if (lAddr.connType == "udp" || lAddr.connType == "tcp") && cAddr.connType == "udp" || cAddr.connType == "tcp" {
		err := fmt.Errorf("Invalid port range")
		if len(lAddr.addr) > 1 && len(cAddr.addr) > 1 && (len(cAddr.addr) != len(lAddr.addr)) {
			fmt.Println(err)
			return err
		} else if len(lAddr.addr) == 1 && len(cAddr.addr) > 1 {
			fmt.Println(err)
			return err
		}
	}

	if C.whoami == C.FORKPROXY_CHILD {
		if lAddr.connType == "unix" && !lAddr.abstract {
			err := os.Remove(lAddr.addr[0])
			if err != nil && !os.IsNotExist(err) {
				return err
			}
		}

		for _, addr := range lAddr.addr {
			file, err := getListenerFile(lAddr.connType, addr)
			if err != nil {
				return err
			}

			err = shared.AbstractUnixSendFd(forkproxyUDSSockFDNum, int(file.Fd()))
			file.Close()
			if err != nil {
				break
			}
		}

		syscall.Close(forkproxyUDSSockFDNum)
		return err
	}

	files := []*os.File{}
	for range lAddr.addr {
		f, err := shared.AbstractUnixReceiveFd(forkproxyUDSSockFDNum)
		if err != nil {
			fmt.Printf("Failed to receive fd from listener process: %v\n", err)
			syscall.Close(forkproxyUDSSockFDNum)
			return err
		}
		files = append(files, f)
	}
	syscall.Close(forkproxyUDSSockFDNum)

	var listenerMap map[int]*lStruct

	isUDPListener := lAddr.connType == "udp"
	listenerMap = make(map[int]*lStruct, len(lAddr.addr))
	if isUDPListener {
		for i, f := range files {
			listenerMap[int(f.Fd())] = &lStruct{
				f:          f,
				lAddrIndex: i,
			}
		}
	} else {
		for i, f := range files {
			listener, err := net.FileListener(f)
			if err != nil {
				fmt.Printf("Failed to re-assemble listener: %v", err)
				return err
			}
			listenerMap[int(f.Fd())] = &lStruct{
				lConn:      &listener,
				lAddrIndex: i,
			}
		}
	}

	// Handle SIGTERM which is sent when the proxy is to be removed
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)

	if cAddr.connType == "unix" && !cAddr.abstract {
		// Create socket
		file, err := getListenerFile("unix", cAddr.addr[0])
		if err != nil {
			return err
		}
		file.Close()

		if cAddr.connType == "unix" && !cAddr.abstract {
			defer os.Remove(cAddr.addr[0])
		}
	}

	if lAddr.connType == "unix" && !lAddr.abstract {
		defer os.Remove(lAddr.addr[0])
	}

	epFd := C.epoll_create1(C.EPOLL_CLOEXEC)
	if epFd < 0 {
		return fmt.Errorf("Failed to create new epoll instance")
	}

	// Wait for SIGTERM and close the listener in order to exit the loop below
	self := syscall.Getpid()
	go func() {
		<-sigs

		for _, f := range files {
			C.epoll_ctl(epFd, C.EPOLL_CTL_DEL, C.int(f.Fd()), nil)
			f.Close()
		}
		syscall.Close(int(epFd))

		for _, l := range listenerMap {
			if isUDPListener {
				conn := (*l).udpConn
				(*conn).Close()
				continue
			}

			conn := (*l).lConn
			(*conn).Close()
		}

		syscall.Kill(self, syscall.SIGKILL)
	}()
	defer syscall.Kill(self, syscall.SIGTERM)

	for _, f := range files {
		var ev C.struct_epoll_event
		ev.events = C.EPOLLIN
		if isUDPListener {
			ev.events |= C.EPOLLONESHOT
		}

		*(*C.int)(unsafe.Pointer(uintptr(unsafe.Pointer(&ev)) + unsafe.Sizeof(ev.events))) = C.int(f.Fd())
		ret := C.epoll_ctl(epFd, C.EPOLL_CTL_ADD, C.int(f.Fd()), &ev)
		if ret < 0 {
			return fmt.Errorf("Failed to add listener fd to epoll instance")
		}
		fmt.Printf("Added listener socket file descriptor %d to epoll instance\n", int(f.Fd()))
	}

	for {
		var events [10]C.struct_epoll_event

		nfds := C.epoll_wait(epFd, &events[0], 10, -1)
		if nfds < 0 {
			fmt.Printf("Failed to wait on epoll instance")
			break
		}

		for i := C.int(0); i < nfds; i++ {
			curFd := *(*C.int)(unsafe.Pointer(uintptr(unsafe.Pointer(&events[i])) + unsafe.Sizeof(events[i].events)))
			srcConn, ok := listenerMap[int(curFd)]
			if !ok {
				continue
			}

			err := listenerInstance(epFd, lAddr, cAddr, curFd, srcConn)
			if err != nil {
				fmt.Printf("Failed to prepare new listener instance: %s", err)
			}
		}
	}

	fmt.Printf("Stopping proxy\n")
	return nil
}

func genericRelay(dst io.ReadWriteCloser, src io.ReadWriteCloser) {
	relayer := func(src io.Writer, dst io.Reader, ch chan error) {
		_, err := io.Copy(eagain.Writer{Writer: src}, eagain.Reader{Reader: dst})
		ch <- err
	}

	chSend := make(chan error)
	go relayer(dst, src, chSend)

	chRecv := make(chan error)
	go relayer(src, dst, chRecv)

	errSnd := <-chSend
	errRcv := <-chRecv

	src.Close()
	dst.Close()

	if errSnd != nil {
		fmt.Printf("Error while sending data %s\n", errSnd)
	}

	if errRcv != nil {
		fmt.Printf("Error while reading data %s\n", errRcv)
	}
}

func unixRelayer(src *net.UnixConn, dst *net.UnixConn, ch chan bool) {
	dataBuf := make([]byte, 4096)
	oobBuf := make([]byte, 4096)

	for {
		// Read from the source
	readAgain:
		sData, sOob, _, _, err := src.ReadMsgUnix(dataBuf, oobBuf)
		if err != nil {
			errno, ok := shared.GetErrno(err)
			if ok && errno == syscall.EAGAIN {
				goto readAgain
			}
			fmt.Printf("Disconnected during read: %v\n", err)
			ch <- true
			return
		}

		var fds []int
		if sOob > 0 {
			entries, err := syscall.ParseSocketControlMessage(oobBuf[:sOob])
			if err != nil {
				fmt.Printf("Failed to parse control message: %v\n", err)
				ch <- true
				return
			}

			for _, msg := range entries {
				fds, err = syscall.ParseUnixRights(&msg)
				if err != nil {
					fmt.Printf("Failed to get fd list for control message: %v\n", err)
					ch <- true
					return
				}
			}
		}

		// Send to the destination
	writeAgain:
		tData, tOob, err := dst.WriteMsgUnix(dataBuf[:sData], oobBuf[:sOob], nil)
		if err != nil {
			errno, ok := shared.GetErrno(err)
			if ok && errno == syscall.EAGAIN {
				goto writeAgain
			}
			fmt.Printf("Disconnected during write: %v\n", err)
			ch <- true
			return
		}

		if sData != tData || sOob != tOob {
			fmt.Printf("Some data got lost during transfer, disconnecting.")
			ch <- true
			return
		}

		// Close those fds we received
		if fds != nil {
			for _, fd := range fds {
				err := syscall.Close(fd)
				if err != nil {
					fmt.Printf("Failed to close fd %d: %v\n", fd, err)
					ch <- true
					return
				}
			}
		}
	}
}

func unixRelay(dst io.ReadWriteCloser, src io.ReadWriteCloser) {
	chSend := make(chan bool)
	go unixRelayer(dst.(*net.UnixConn), src.(*net.UnixConn), chSend)

	chRecv := make(chan bool)
	go unixRelayer(src.(*net.UnixConn), dst.(*net.UnixConn), chRecv)

	<-chSend
	<-chRecv

	src.Close()
	dst.Close()
}

func tryListen(protocol string, addr string) (net.Listener, error) {
	var listener net.Listener
	var err error

	for i := 0; i < 10; i++ {
		listener, err = net.Listen(protocol, addr)
		if err == nil {
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	if err != nil {
		return nil, err
	}

	return listener, nil
}

func tryListenUDP(protocol string, addr string) (*os.File, error) {
	var UDPConn *net.UDPConn
	var err error

	udpAddr, err := net.ResolveUDPAddr(protocol, addr)
	if err != nil {
		return nil, err
	}

	for i := 0; i < 10; i++ {
		UDPConn, err = net.ListenUDP(protocol, udpAddr)
		if err == nil {
			file, err := UDPConn.File()
			UDPConn.Close()
			return file, err
		}

		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}

	if UDPConn == nil {
		return nil, fmt.Errorf("Failed to setup UDP listener")
	}

	file, err := UDPConn.File()
	UDPConn.Close()
	return file, err
}

func getListenerFile(protocol string, addr string) (*os.File, error) {
	if protocol == "udp" {
		return tryListenUDP("udp", addr)
	}

	listener, err := tryListen(protocol, addr)
	if err != nil {
		return nil, fmt.Errorf("Failed to listen on %s: %v", addr, err)
	}

	file := &os.File{}
	switch listener.(type) {
	case *net.TCPListener:
		tcpListener := listener.(*net.TCPListener)
		file, err = tcpListener.File()
	case *net.UnixListener:
		unixListener := listener.(*net.UnixListener)
		file, err = unixListener.File()
	}
	if err != nil {
		return nil, fmt.Errorf("Failed to get file from listener: %v", err)
	}

	return file, nil
}

func parsePortRange(r string) (int64, int64, error) {
	entries := strings.Split(r, "-")
	if len(entries) > 2 {
		return -1, -1, fmt.Errorf("Invalid port range %s", r)
	}

	base, err := strconv.ParseInt(entries[0], 10, 64)
	if err != nil {
		return -1, -1, err
	}

	size := int64(1)
	if len(entries) > 1 {
		size, err = strconv.ParseInt(entries[1], 10, 64)
		if err != nil {
			return -1, -1, err
		}

		size -= base
		size += 1
	}

	return base, size, nil
}

func parseAddr(addr string) (*proxyAddress, error) {
	// Split into <protocol> and <address>
	fields := strings.SplitN(addr, ":", 2)

	newProxyAddr := &proxyAddress{
		connType: fields[0],
		abstract: strings.HasPrefix(fields[1], "@"),
	}

	// unix addresses cannot have ports
	if newProxyAddr.connType == "unix" {
		newProxyAddr.addr = []string{fields[1]}
		return newProxyAddr, nil
	}

	// Split <address> into <address> and <ports>
	addrParts := strings.SplitN(fields[1], ":", 2)
	// no ports
	if len(addrParts) == 1 {
		newProxyAddr.addr = []string{fields[1]}
		return newProxyAddr, nil
	}

	// Split <ports> into individual ports and port ranges
	ports := strings.SplitN(addrParts[1], ",", -1)
	for _, port := range ports {
		portFirst, portRange, err := parsePortRange(port)
		if err != nil {
			return nil, err
		}

		for i := int64(0); i < portRange; i++ {
			newAddr := fmt.Sprintf("%s:%d", addrParts[0], portFirst+i)
			newProxyAddr.addr = append(newProxyAddr.addr, newAddr)
		}
	}

	return newProxyAddr, nil
}
