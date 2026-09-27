// shioriko-probe prints diagnostics about the attached MTP device.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"shioriko/internal/mtpdev"
)

func main() {
	verbose := flag.Bool("v", false, "verbose MTP logging")
	readTest := flag.Bool("read", false, "read the first 4MiB of the first regular file found")
	flag.Parse()

	dev, err := mtpdev.Open("")
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer dev.Close()
	dev.Verbose = *verbose

	fmt.Printf("Device      : %s\n", dev.Name)
	fmt.Printf("Manufacturer: %s  Model: %s  Version: %s  Serial: %s\n", dev.Info.Manufacturer, dev.Info.Model, dev.Info.DeviceVersion, dev.Info.SerialNumber)
	fmt.Printf("Extension   : %s\n", dev.Info.MTPExtension)
	fmt.Printf("PartialRead64=%v PartialWrite=%v Move=%v GetObjectPropList=%v GetPartialObject=%v\n",
		dev.Supports(0x95C1), dev.SupportsPartialWrite(), dev.SupportsMove(), dev.Supports(0x9805), dev.Supports(0x101B))

	stores, err := dev.Storages()
	if err != nil {
		log.Fatalf("storages: %v", err)
	}
	if len(stores) == 0 {
		fmt.Println("No storages visible. Unlock the phone and select 'File transfer' USB mode.")
		os.Exit(1)
	}
	var firstFile *mtpdev.Object
	for _, s := range stores {
		fmt.Printf("Storage %08X %q cap=%.1fGB free=%.1fGB ro=%v\n", s.ID, s.Description, float64(s.Capacity)/1e9, float64(s.Free)/1e9, s.ReadOnly)
		t0 := time.Now()
		objs, err := dev.List(s.ID, mtpdev.ParentRoot)
		if err != nil {
			log.Fatalf("list root: %v", err)
		}
		fmt.Printf("  root: %d entries in %v\n", len(objs), time.Since(t0))
		for _, o := range objs {
			fmt.Printf("    %-4s %10d %s %s\n", kind(o), o.Size, o.ModTime.Format("2006-01-02 15:04:05"), o.Name)
			if !o.IsDir && firstFile == nil {
				c := o
				firstFile = &c
			}
		}
		for _, o := range objs {
			if o.IsDir {
				t0 = time.Now()
				sub, err := dev.List(s.ID, o.Handle)
				if err != nil {
					fmt.Printf("  %s: list error: %v\n", o.Name, err)
					continue
				}
				fmt.Printf("  %s: %d entries in %v\n", o.Name, len(sub), time.Since(t0))
				for _, so := range sub {
					if !so.IsDir && (firstFile == nil || so.Size > firstFile.Size) && so.Size < 64<<20 {
						c := so
						firstFile = &c
					}
				}
			}
		}
	}
	if *readTest && firstFile != nil {
		want := int(min64(firstFile.Size, 4<<20))
		fmt.Printf("Reading %d bytes of %q ...\n", want, firstFile.Name)
		cw := &countWriter{}
		t0 := time.Now()
		if err := dev.ReadAt(firstFile.Handle, 0, want, cw); err != nil {
			log.Fatalf("read: %v", err)
		}
		el := time.Since(t0)
		fmt.Printf("  got %d bytes in %v (%.1f MB/s)\n", cw.n, el, float64(cw.n)/el.Seconds()/1e6)
	}
}

func kind(o mtpdev.Object) string {
	if o.IsDir {
		return "dir"
	}
	return "file"
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

var _ io.Writer = (*countWriter)(nil)
