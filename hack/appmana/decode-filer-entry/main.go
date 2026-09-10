// decode-filer-entry prints filer store values (as stored in etcd, leveldb,
// redis, ...) as JSON. Feed it one base64 value per line on stdin, for example
// the "value" field of `etcdctl get -w json`.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "base64: %v\n", err)
			continue
		}
		entry := &filer.Entry{}
		if err := entry.DecodeAttributesAndChunks(util.MaybeDecompressData(raw)); err != nil {
			fmt.Fprintf(os.Stderr, "decode: %v\n", err)
			continue
		}
		extended := map[string]string{}
		for k, v := range entry.Extended {
			extended[k] = string(v)
		}
		out, _ := json.Marshal(map[string]any{
			"mtime": entry.Attr.Mtime.UTC().Format("2006-01-02T15:04:05Z"), "mode": entry.Attr.Mode.String(),
			"inode": entry.Attr.Inode, "extended": extended, "chunks": len(entry.GetChunks()),
		})
		fmt.Println(string(out))
	}
}
