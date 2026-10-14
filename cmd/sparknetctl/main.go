// Command sparknetctl is the goSpark operator CLI: it mints and inspects node
// identities, signs and verifies envelopes, asks the router where a prompt
// should run, and streams an NVFP4 block to a peer's data plane.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/kvtransfer"
	"github.com/reallife-spark/goSpark/internal/router"
	"github.com/reallife-spark/goSpark/internal/transport"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "fingerprint":
		err = cmdFingerprint(os.Args[2:])
	case "caps":
		err = cmdCaps()
	case "route":
		err = cmdRoute(os.Args[2:])
	case "decode-block":
		err = cmdDecodeBlock(os.Args[2:])
	case "encode-block":
		err = cmdEncodeBlock(os.Args[2:])
	case "send-block":
		err = cmdSendBlock(os.Args[2:])
	case "ops":
		err = cmdOps()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sparknetctl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sparknetctl — goSpark control-plane CLI

Usage:
  sparknetctl keygen     -node ID -owner ID -out FILE
  sparknetctl fingerprint -key FILE
  sparknetctl caps
  sparknetctl ops
  sparknetctl route      -prompt "..." [-caps a,b]
  sparknetctl encode-block -values 1,-2,0.5 -out BLOCK.canonical
  sparknetctl decode-block -file BLOCK.canonical
  sparknetctl send-block  -addr HOST:PORT -file BLOCK.canonical [-block-id ID] [-origin NODE] [-pin FINGERPRINT]
`)
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	node := fs.String("node", "", "node id")
	owner := fs.String("owner", "", "owner id")
	out := fs.String("out", "", "output key file")
	_ = fs.Parse(args)
	if *node == "" || *out == "" {
		return fmt.Errorf("keygen: -node and -out are required")
	}
	id, err := identity.Generate(*node, *owner)
	if err != nil {
		return err
	}
	if err := id.Save(*out); err != nil {
		return err
	}
	fmt.Printf("node=%s\nfingerprint=%s\nkey_file=%s\npublic_key_b64=%s\n",
		id.NodeID, id.Fingerprint(), *out, id.PublicKeyB64())
	fmt.Fprintln(os.Stderr, "warning: the key file holds the private half; keep it 0600 and never commit it")
	return nil
}

func cmdFingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	key := fs.String("key", "", "key file")
	_ = fs.Parse(args)
	if *key == "" {
		return fmt.Errorf("fingerprint: -key is required")
	}
	id, err := identity.Load(*key)
	if err != nil {
		return err
	}
	fmt.Printf("node=%s\nowner=%s\nfingerprint=%s\n", id.NodeID, id.OwnerID, id.Fingerprint())
	return nil
}

func cmdCaps() error {
	for _, c := range authz.KnownSorted() {
		tag := ""
		if authz.IsSensitive(c) {
			tag = "\t(sensitive)"
		}
		fmt.Printf("%s%s\n", c, tag)
	}
	return nil
}

func cmdOps() error {
	for _, op := range []string{"arith", "digest"} {
		fmt.Println(op)
	}
	return nil
}

func cmdRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	prompt := fs.String("prompt", "", "prompt text")
	caps := fs.String("caps", "", "comma-separated capabilities this node holds")
	_ = fs.Parse(args)
	if *prompt == "" {
		return fmt.Errorf("route: -prompt is required")
	}
	r := router.New(router.Config{})
	dec := r.Decide(*prompt, splitCSV(*caps))
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(dec)
}

func cmdEncodeBlock(args []string) error {
	fs := flag.NewFlagSet("encode-block", flag.ExitOnError)
	values := fs.String("values", "", "comma-separated float values")
	out := fs.String("out", "", "output canonical block file (default stdout summary)")
	_ = fs.Parse(args)
	if *values == "" {
		return fmt.Errorf("encode-block: -values is required")
	}
	floats, err := parseFloats(*values)
	if err != nil {
		return err
	}
	b, err := (kvtransfer.ReferenceCodec{}).Encode(floats)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, b.Canonical(), 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("elements=%d blocks=%d global_scale=%g digest=%s\n",
		b.NumElements, len(b.BlockScale), b.GlobalScale, b.Digest())
	return nil
}

func parseFloats(s string) ([]float32, error) {
	parts := splitCSV(s)
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, fmt.Errorf("encode-block: bad value %q: %w", p, err)
		}
		out = append(out, float32(v))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("encode-block: no values parsed")
	}
	return out, nil
}

func cmdDecodeBlock(args []string) error {
	fs := flag.NewFlagSet("decode-block", flag.ExitOnError)
	file := fs.String("file", "", "canonical block file")
	_ = fs.Parse(args)
	if *file == "" {
		return fmt.Errorf("decode-block: -file is required")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	b, err := kvtransfer.DecodeCanonical(data)
	if err != nil {
		return err
	}
	codec := kvtransfer.ReferenceCodec{}
	vals, err := codec.Decode(b)
	if err != nil {
		return err
	}
	var first []float32
	if len(vals) > 4 {
		first = vals[:4]
	} else {
		first = vals
	}
	fmt.Printf("elements=%d blocks=%d global_scale=%g digest=%s\nfirst=%v\n",
		b.NumElements, len(b.BlockScale), b.GlobalScale, b.Digest(), first)
	return nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func cmdSendBlock(args []string) error {
	fs := flag.NewFlagSet("send-block", flag.ExitOnError)
	addr := fs.String("addr", "", "peer data-plane address host:port")
	file := fs.String("file", "", "canonical block file")
	blockID := fs.String("block-id", "", "block id (default: derived from digest)")
	origin := fs.String("origin", "sparknetctl", "origin node id")
	chunk := fs.Int("chunk", 0, "chunk size in bytes (0 = default)")
	pin := fs.String("pin", "", "expected peer certificate SHA-256 fingerprint (hex); required outside single-process use")
	timeout := fs.Duration("timeout", 30*time.Second, "overall timeout")
	_ = fs.Parse(args)
	if *addr == "" || *file == "" {
		return fmt.Errorf("send-block: -addr and -file are required")
	}
	data, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	b, err := kvtransfer.DecodeCanonical(data)
	if err != nil {
		return err
	}
	id := *blockID
	if id == "" {
		id = b.Digest()[:16]
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ack, err := transport.SendBlockWithOptions(ctx, *addr, id, *origin, b, *chunk, transport.DialOptions{
		PinnedFingerprint: *pin,
	})
	if err != nil {
		return err
	}
	fmt.Printf("sent block_id=%s elements=%d digest=%s\nack=%s\n", id, b.NumElements, b.Digest(), ack)
	return nil
}
