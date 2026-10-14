// Command sparknetctl is the goSpark operator CLI: it mints and inspects node
// identities, signs and verifies envelopes, asks the router where a prompt
// should run, and streams an NVFP4 block to a peer's data plane.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/identity"
	"github.com/reallife-spark/goSpark/internal/kvtransfer"
	"github.com/reallife-spark/goSpark/internal/registryfile"
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
	case "registry":
		err = cmdRegistry(os.Args[2:])
	case "admin":
		err = cmdAdmin(os.Args[2:])
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
  sparknetctl keygen     -node ID [-role master|peer] [-overlay IPV4] [-caps a,b] -out FILE
  sparknetctl fingerprint -key FILE
  sparknetctl caps
  sparknetctl ops
  sparknetctl registry sign   -master-key FILE (-authorizations FILE | -node-key FILE -caps a,b) -out FILE [-ttl 8760h] [-issued-at RFC3339]
  sparknetctl registry verify -registry FILE (-master-pub B64 | -master-key FILE)
  sparknetctl registry show   -registry FILE
  sparknetctl registry revoke -master-key FILE -registry FILE -node ID [-unrevoke] [-out FILE]
  sparknetctl admin show  [-addr URL]
  sparknetctl admin revoke -node ID [-unrevoke] [-addr URL]
  sparknetctl admin authorize -node ID -pubkey B64 -caps a,b [-role peer] [-overlay IP] [-ttl 8760h] [-addr URL]
  sparknetctl route      -prompt "..." [-caps a,b]
  sparknetctl encode-block -values 1,-2,0.5 -out BLOCK.canonical
  sparknetctl decode-block -file BLOCK.canonical
  sparknetctl send-block  -addr HOST:PORT -file BLOCK.canonical [-block-id ID] [-origin NODE] [-pin FINGERPRINT]
`)
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	node := fs.String("node", "", "node id")
	role := fs.String("role", identity.RolePeer, "node role: master|peer")
	overlay := fs.String("overlay", "", "overlay IPv4 address")
	caps := fs.String("caps", "", "comma-separated requested capabilities")
	out := fs.String("out", "", "output key file")
	_ = fs.Parse(args)
	if *node == "" || *out == "" {
		return fmt.Errorf("keygen: -node and -out are required")
	}
	id, err := identity.Generate(*node, *role)
	if err != nil {
		return err
	}
	id.OverlayIPv4 = *overlay
	id.RequestedCapabilities = splitCSV(*caps)
	if err := id.Save(*out); err != nil {
		return err
	}
	fmt.Printf("node=%s\nrole=%s\nfingerprint=%s\nkey_file=%s\npublic_key_b64=%s\n",
		id.NodeID, id.Role, id.Fingerprint(), *out, id.PublicKeyB64())
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
	fmt.Printf("node=%s\nrole=%s\nfingerprint=%s\n", id.NodeID, id.Role, id.Fingerprint())
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

// cmdRegistry signs, verifies and inspects Master-signed node registries.
func cmdRegistry(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("registry: expected sign|verify|show")
	}
	switch args[0] {
	case "sign":
		return cmdRegistrySign(args[1:])
	case "verify":
		return cmdRegistryVerify(args[1:])
	case "show":
		return cmdRegistryShow(args[1:])
	case "revoke":
		return cmdRegistryRevoke(args[1:])
	default:
		return fmt.Errorf("registry: unknown action %q (want sign|verify|show|revoke)", args[0])
	}
}

func cmdRegistrySign(args []string) error {
	fs := flag.NewFlagSet("registry sign", flag.ExitOnError)
	masterKey := fs.String("master-key", "", "master private key file")
	authFile := fs.String("authorizations", "", "JSON file: array of authorizations or a {authorizations:[...]} object")
	nodeKey := fs.String("node-key", "", "convenience: build a single entry from this node key file")
	caps := fs.String("caps", "", "comma-separated capabilities for -node-key")
	out := fs.String("out", "", "output registry file (default stdout)")
	ttl := fs.Duration("ttl", authz.DefaultAuthorizationTTL, "authorization lifetime (Go duration)")
	issuedRaw := fs.String("issued-at", "", "issue time (RFC3339); default now")
	_ = fs.Parse(args)
	if *masterKey == "" {
		return fmt.Errorf("registry sign: -master-key is required")
	}
	master, err := identity.Load(*masterKey)
	if err != nil {
		return err
	}
	issued := time.Now()
	if *issuedRaw != "" {
		issued, err = time.Parse(time.RFC3339, *issuedRaw)
		if err != nil {
			return fmt.Errorf("registry sign: -issued-at: %w", err)
		}
	}
	var entries []apiv1.NodeAuthorization
	switch {
	case *authFile != "":
		entries, err = loadAuthorizations(*authFile)
		if err != nil {
			return err
		}
	case *nodeKey != "":
		id, err := identity.Load(*nodeKey)
		if err != nil {
			return err
		}
		entry, err := authz.NewAuthorization(master, id.Projection(), splitCSV(*caps), issued, issued.Add(*ttl))
		if err != nil {
			return err
		}
		entries = []apiv1.NodeAuthorization{entry}
	default:
		return fmt.Errorf("registry sign: need -authorizations FILE or -node-key FILE")
	}
	reg := &apiv1.NodeRegistry{Authorizations: entries}
	if err := authz.SignRegistry(master, reg); err != nil {
		return err
	}
	digest, err := authz.RegistryDigest(reg)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	} else {
		fmt.Println(string(data))
	}
	fmt.Printf("nodes=%d digest=%s\n", len(reg.Authorizations), digest)
	return nil
}

func cmdRegistryRevoke(args []string) error {
	fs := flag.NewFlagSet("registry revoke", flag.ExitOnError)
	masterKey := fs.String("master-key", "", "master private key file")
	registry := fs.String("registry", "", "input registry file")
	node := fs.String("node", "", "node id to revoke")
	unrevoke := fs.Bool("unrevoke", false, "clear the revocation instead (reinstatement)")
	out := fs.String("out", "", "output registry file (default stdout)")
	_ = fs.Parse(args)
	if *masterKey == "" || *registry == "" || *node == "" {
		return fmt.Errorf("registry revoke: -master-key, -registry and -node are required")
	}
	master, err := identity.Load(*masterKey)
	if err != nil {
		return err
	}
	reg, err := registryfile.Load(*registry)
	if err != nil {
		return err
	}
	if err := authz.SetRevocation(reg, *node, !*unrevoke); err != nil {
		return err
	}
	if err := authz.SignRegistry(master, reg); err != nil {
		return err
	}
	entry, _ := authz.NewRegistry(reg).Get(*node)
	state := "revoked"
	if !entry.Revoked {
		state = "reinstated"
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	} else {
		fmt.Println(string(data))
	}
	digest, _ := authz.RegistryDigest(reg)
	view := authz.NewRegistry(reg)
	fmt.Fprintf(os.Stderr, "%s node=%s latest_issued=%s digest=%s\n",
		state, *node, view.LatestIssued().Format(time.RFC3339), digest)
	return nil
}

// cmdAdmin talks to a running control plane's loopback admin surface, which
// mutates the live registry the way `registry revoke` mutates one on disk.
func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("admin: expected show|revoke|authorize")
	}
	switch args[0] {
	case "show":
		return cmdAdminShow(args[1:])
	case "revoke":
		return cmdAdminRevoke(args[1:])
	case "authorize":
		return cmdAdminAuthorize(args[1:])
	default:
		return fmt.Errorf("admin: unknown action %q (want show|revoke|authorize)", args[0])
	}
}

func cmdAdminShow(args []string) error {
	fs := flag.NewFlagSet("admin show", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:8899", "control-plane admin base URL")
	_ = fs.Parse(args)
	return adminRequest(*addr, http.MethodGet, "/admin/registry", nil)
}

func cmdAdminRevoke(args []string) error {
	fs := flag.NewFlagSet("admin revoke", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:8899", "control-plane admin base URL")
	node := fs.String("node", "", "node id")
	unrevoke := fs.Bool("unrevoke", false, "reinstate instead of revoke")
	_ = fs.Parse(args)
	if *node == "" {
		return fmt.Errorf("admin revoke: -node is required")
	}
	body, err := json.Marshal(map[string]any{"node_id": *node, "revoked": !*unrevoke})
	if err != nil {
		return err
	}
	return adminRequest(*addr, http.MethodPost, "/admin/registry/revoke", body)
}

func cmdAdminAuthorize(args []string) error {
	fs := flag.NewFlagSet("admin authorize", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:8899", "control-plane admin base URL")
	node := fs.String("node", "", "node id")
	pub := fs.String("pubkey", "", "node public key, base64 (the wireguard_public_key_b64 slot)")
	role := fs.String("role", identity.RolePeer, "node role: master|peer")
	overlay := fs.String("overlay", "", "node overlay IPv4")
	caps := fs.String("caps", "", "comma-separated capabilities to grant")
	reqCaps := fs.String("req-caps", "", "projection requested_capabilities (default: -caps)")
	ttl := fs.Duration("ttl", authz.DefaultAuthorizationTTL, "authorization lifetime (Go duration)")
	_ = fs.Parse(args)
	if *node == "" || *pub == "" || *caps == "" {
		return fmt.Errorf("admin authorize: -node, -pubkey and -caps are required")
	}
	if _, err := identity.ParsePublicKeyB64(*pub); err != nil {
		return err
	}
	requested := *reqCaps
	if requested == "" {
		requested = *caps
	}
	body, err := json.Marshal(map[string]any{
		"identity": apiv1.NodeIdentity{
			NodeID:                *node,
			Role:                  *role,
			WireGuardPublicKeyB64: *pub,
			OverlayIPv4:           *overlay,
			RequestedCapabilities: splitCSV(requested),
		},
		"capabilities": splitCSV(*caps),
		"ttl_seconds":  int64(ttl.Seconds()),
	})
	if err != nil {
		return err
	}
	return adminRequest(*addr, http.MethodPost, "/admin/registry/authorize", body)
}

// adminRequest performs one admin call and prints the JSON response, turning a
// non-200 into an error that carries the server's message.
func adminRequest(addr, method, path string, body []byte) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, strings.TrimRight(addr, "/")+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	var out any
	if json.Unmarshal(data, &out) == nil {
		pretty, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(pretty))
		return nil
	}
	fmt.Println(string(data))
	return nil
}

func cmdRegistryVerify(args []string) error {
	fs := flag.NewFlagSet("registry verify", flag.ExitOnError)
	regFile := fs.String("registry", "", "registry file")
	masterPub := fs.String("master-pub", "", "master public key (base64)")
	masterKey := fs.String("master-key", "", "master key file (alternative to -master-pub)")
	_ = fs.Parse(args)
	if *regFile == "" {
		return fmt.Errorf("registry verify: -registry is required")
	}
	pub, err := resolveMasterPub(*masterPub, *masterKey)
	if err != nil {
		return err
	}
	reg, err := loadRegistry(*regFile)
	if err != nil {
		return err
	}
	if err := authz.VerifyRegistry(pub, reg); err != nil {
		return err
	}
	digest, _ := authz.RegistryDigest(reg)
	fmt.Printf("OK nodes=%d digest=%s latest_issued=%s\n",
		len(reg.Authorizations), digest, authz.NewRegistry(reg).LatestIssued().Format(time.RFC3339))
	return nil
}

func cmdRegistryShow(args []string) error {
	fs := flag.NewFlagSet("registry show", flag.ExitOnError)
	regFile := fs.String("registry", "", "registry file")
	_ = fs.Parse(args)
	if *regFile == "" {
		return fmt.Errorf("registry show: -registry is required")
	}
	reg, err := loadRegistry(*regFile)
	if err != nil {
		return err
	}
	digest, _ := authz.RegistryDigest(reg)
	fmt.Printf("schema=%s master=%s latest_issued=%s nodes=%d digest=%s\n",
		reg.SchemaVersion, reg.MasterPublicKeyB64,
		authz.NewRegistry(reg).LatestIssued().Format(time.RFC3339), len(reg.Authorizations), digest)
	for _, e := range reg.Authorizations {
		state := "active"
		if e.Revoked {
			state = "REVOKED"
		}
		fmt.Printf("  %s\t%s\tcaps=%v\tfp=%s\texpires=%s\n",
			e.Identity.NodeID, state, e.Capabilities, short(e.Fingerprint), e.ExpiresAt)
	}
	return nil
}

func loadRegistry(path string) (*apiv1.NodeRegistry, error) {
	return registryfile.Load(path)
}

// loadAuthorizations accepts either a bare JSON array of authorizations or an
// object with an "authorizations" field.
func loadAuthorizations(path string) ([]apiv1.NodeAuthorization, error) {
	return registryfile.LoadAuthorizations(path)
}

func resolveMasterPub(pubB64, keyFile string) (ed25519.PublicKey, error) {
	switch {
	case pubB64 != "":
		return identity.ParsePublicKeyB64(pubB64)
	case keyFile != "":
		id, err := identity.Load(keyFile)
		if err != nil {
			return nil, err
		}
		return id.Public, nil
	default:
		return nil, fmt.Errorf("need -master-pub or -master-key")
	}
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

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
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
