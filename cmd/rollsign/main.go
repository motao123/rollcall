// Command rollsign 生成 Agent 自更新所需的 Ed25519 密钥与签名清单。
//
// 用法：
//
//	rollsign keygen -out rollsign.key
//	rollsign sign -key rollsign.key -version 1.7.0 \
//	  -base-url https://example.com/releases/v1.7.0 \
//	  -asset linux-amd64=dist/rollcall-agent-linux-amd64 \
//	  -asset linux-arm64=dist/rollcall-agent-linux-arm64 \
//	  -out manifest.json
//
// 私钥只保存在本机，绝不能提交到仓库；公钥填入 Agent 配置的 update.publicKey。
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/motao/rollcall/internal/selfupdate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "keygen":
		keygen(os.Args[2:])
	case "sign":
		sign(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rollsign — Rollcall Agent 自更新签名工具

  rollsign keygen [-out rollsign.key]
      生成 Ed25519 密钥，打印 base64 公钥；私钥文件权限 0600。

  rollsign sign -key rollsign.key -version v1.7.0 -base-url URL -asset 平台=文件 ... [-out manifest.json]
      计算每个资产的 SHA-256 与大小并生成签名清单。
      -asset 可重复，平台键如 linux-amd64、linux-arm64。
      -allow-insecure 仅用于本机 HTTP 测试。
`)
}

func keygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "rollsign.key", "私钥文件路径")
	_ = fs.Parse(args)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal("生成密钥失败: %v", err)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fatal("创建私钥文件失败: %v", err)
	}
	if _, err := fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv)); err != nil {
		_ = f.Close()
		fatal("写入私钥失败: %v", err)
	}
	if err := f.Close(); err != nil {
		fatal("关闭私钥文件失败: %v", err)
	}
	_ = os.Chmod(*out, 0o600)

	fmt.Printf("私钥: %s（权限 0600，切勿提交或外传）\n", *out)
	fmt.Printf("公钥: %s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Println("把上面的公钥填入 agent.json 的 update.publicKey")
}

type assetList []string

func (a *assetList) String() string { return strings.Join(*a, ",") }
func (a *assetList) Set(v string) error {
	key, path, ok := strings.Cut(v, "=")
	if !ok || key == "" || path == "" {
		return fmt.Errorf("格式应为 平台=文件路径")
	}
	if !selfupdate.ValidAssetKey(key) {
		return fmt.Errorf("平台键无效: %q", key)
	}
	if len(*a) >= 8 {
		return fmt.Errorf("资产数量过多")
	}
	*a = append(*a, v)
	return nil
}

func sign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "rollsign.key", "私钥文件")
	ver := fs.String("version", "", "发布版本，如 v1.7.0")
	baseURL := fs.String("base-url", "", "资产下载地址前缀")
	published := fs.Int64("published-at", time.Now().Unix(), "发布时间（unix 秒）")
	out := fs.String("out", "", "输出文件；留空打印到 stdout")
	allowInsecure := fs.Bool("allow-insecure", false, "允许 HTTP 资产地址（仅本机测试）")
	var assets assetList
	fs.Var(&assets, "asset", "平台=文件路径（可重复）")
	_ = fs.Parse(args)

	if strings.TrimSpace(*ver) == "" || !selfupdate.ValidVersion(*ver) {
		fatal("-version 必填且必须是合法版本号")
	}
	if strings.TrimSpace(*baseURL) == "" {
		fatal("-base-url 必填")
	}
	if len(assets) == 0 {
		fatal("至少提供一个 -asset")
	}
	// 必须在读取私钥、计算哈希之前拦下危险输出路径：
	// os.WriteFile 会直接截断目标文件，-out 指向私钥或资产会静默毁掉它，
	// 而命令仍会打印「已写入」。
	if *out != "" {
		if err := checkOutputPath(*out, *keyPath, assets); err != nil {
			fatal("%v", err)
		}
	}

	priv := loadKey(*keyPath)
	m, err := buildManifest(assets, *ver, *published, *baseURL, *allowInsecure)
	if err != nil {
		fatal("%v", err)
	}

	env, err := selfupdate.SignManifest(m, priv)
	if err != nil {
		fatal("签名失败: %v", err)
	}
	if *out == "" {
		fmt.Println(string(env))
		return
	}
	if err := os.WriteFile(*out, append(env, '\n'), 0o644); err != nil {
		fatal("写入清单失败: %v", err)
	}
	fmt.Printf("已写入 %s（版本 %s，%d 个资产）\n", *out, m.Version, len(m.Assets))
}

// checkOutputPath 拒绝把 -out 指向私钥或任一资产文件。
// 这两类文件被 os.WriteFile 截断后无法恢复：私钥丢失意味着再也无法签发更新，
// 资产被覆盖意味着已计算的 SHA-256 与实际文件对不上。
func checkOutputPath(out, key string, assets []string) error {
	if samePath(out, key) {
		return fmt.Errorf("-out 不能指向私钥文件: %s", out)
	}
	for _, spec := range assets {
		_, path, _ := strings.Cut(spec, "=")
		if samePath(out, path) {
			return fmt.Errorf("-out 不能覆盖资产文件: %s", path)
		}
	}
	return nil
}

// samePath 判断两个路径是否指向同一文件。
// 同时存在时用 os.SameFile（可识别硬链接），否则退化为清理后的绝对路径比较。
func samePath(a, b string) bool {
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if fi, err := os.Stat(aa); err == nil {
		if fj, err := os.Stat(bb); err == nil {
			return os.SameFile(fi, fj)
		}
	}
	return strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

// buildManifest 计算每个资产的 SHA-256 与大小，组装待签名清单。
//
// 同一平台键重复时必须报错：map 赋值会静默覆盖，前一个资产被丢弃，
// 签出的清单看起来完全正常却缺少一个架构 —— 而清单一旦签名发布，
// 那个平台就再也收不到更新。
func buildManifest(assets []string, version string, publishedAt int64, baseURL string, allowInsecure bool) (*selfupdate.Manifest, error) {
	m := &selfupdate.Manifest{
		Version:     strings.TrimPrefix(version, "v"),
		PublishedAt: publishedAt,
		Assets:      make(map[string]selfupdate.Asset, len(assets)),
	}
	baseNames := map[string]string{}
	for _, spec := range assets {
		key, path, _ := strings.Cut(spec, "=")
		if _, dup := m.Assets[key]; dup {
			return nil, fmt.Errorf("-asset 平台键重复: %s", key)
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("资产 %s 不可读: %w", key, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("资产 %s 不是普通文件: %s", key, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取资产 %s 失败: %w", key, err)
		}
		// 用实际读到的字节数而非 Stat 的大小：两者之间文件可能被替换，
		// 清单里的 Size 必须与 SHA-256 覆盖的内容一致。
		size := int64(len(b))
		if size < selfupdate.MinAssetSize || size > selfupdate.MaxAssetSize {
			return nil, fmt.Errorf("资产 %s 大小 %d 超出允许范围", key, size)
		}
		base := filepath.Base(path)
		if prev, dup := baseNames[base]; dup {
			return nil, fmt.Errorf("资产 %s 与 %s 的文件名相同（%s），下载地址会冲突", key, prev, base)
		}
		baseNames[base] = key
		sum := sha256.Sum256(b)
		url := strings.TrimRight(baseURL, "/") + "/" + base
		if err := selfupdate.CheckURL(url, allowInsecure); err != nil {
			return nil, fmt.Errorf("资产 %s 地址无效: %v", key, err)
		}
		m.Assets[key] = selfupdate.Asset{
			URL: url, SHA256: hex.EncodeToString(sum[:]), Size: size,
		}
	}
	return m, nil
}

func loadKey(path string) ed25519.PrivateKey {
	fi, err := os.Stat(path)
	if err != nil {
		fatal("读取私钥失败: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		fatal("私钥文件权限过宽（需 0600）: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fatal("读取私钥失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		fatal("私钥格式无效（应为 base64 编码的 64 字节 Ed25519 私钥）")
	}
	return ed25519.PrivateKey(raw)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "rollsign: "+format+"\n", args...)
	os.Exit(1)
}
