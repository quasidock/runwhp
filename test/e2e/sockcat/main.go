// Command sockcat is a Unix socket client for the end-to-end tests, which
// alpine's busybox lacks: it sends its input to the socket, half-closes,
// and writes what comes back.
//
//	sockcat PATH            send stdin, then print the answer
//	sockcat -n N PATH MSG   N connections at once, each sending MSG and
//	                        checking it comes back as the echo server
//	                        answers it, "got:MSG"
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

func main() {
	n := flag.Int("n", 0, "connections at once, each sending MSG")
	flag.Parse()
	if *n > 0 {
		if flag.NArg() != 2 {
			fail("usage: sockcat -n N PATH MSG")
		}
		concurrent(*n, flag.Arg(0), flag.Arg(1))
		return
	}
	if flag.NArg() != 1 {
		fail("usage: sockcat PATH")
	}
	c, err := dial(flag.Arg(0))
	if err != nil {
		fail("%v", err)
	}
	if _, err := io.Copy(c, os.Stdin); err != nil {
		fail("sending: %v", err)
	}
	c.CloseWrite()
	if _, err := io.Copy(os.Stdout, c); err != nil {
		fail("receiving: %v", err)
	}
}

func dial(p string) (*net.UnixConn, error) {
	return net.DialUnix("unix", nil, &net.UnixAddr{Name: p, Net: "unix"})
}

func concurrent(n int, p, msg string) {
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dial(p)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			m := fmt.Sprintf("%s-%d", msg, i)
			io.WriteString(c, m)
			c.CloseWrite()
			got, err := io.ReadAll(c)
			if err != nil || string(got) != "got:"+m {
				errs <- fmt.Errorf("connection %d: got %q, %v", i, got, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	bad := 0
	for err := range errs {
		fmt.Fprintln(os.Stderr, err)
		bad++
	}
	if bad > 0 {
		fail("%d of %d connections failed", bad, n)
	}
	fmt.Printf("%d connections answered\n", n)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "sockcat: "+format+"\n", a...)
	os.Exit(1)
}
