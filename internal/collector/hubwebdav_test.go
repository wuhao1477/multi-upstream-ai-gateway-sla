package collector

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// 夹具是 **all-api-hub 自己的** encryptWebdavBackupContent 产出的，
// 不是我方写一个加密器再解开自己 —— 那种自洽只能证明两边一起错。
//
// 文件与出处：verify/fixtures/all-api-hub-encrypted-backup.json 里的 _provenance；
// 重新生成用 verify/gen-hub-envelope.mjs（它 import 的是上游源码本体）。
//
// 从文件读而不是内联一份常量：同一段密文另有一个消费方（verify/ui-stack.sh
// 把它 PUT 到真 WebDAV 容器上），抄成两份迟早对不上，而对不上的那一天
// 两边的断言都还是绿的。
//
// 明文是**合成的**小备份：被测对象是信封格式与 AES-GCM 解密，不是备份内容，
// 而拿真备份来加密等于把真凭证以密文形式写进仓库。
type hubFixture struct {
	Password  string          `json:"password"`
	Plaintext string          `json:"plaintext"`
	Envelope  json.RawMessage `json:"envelope"`
}

func loadHubFixture(t *testing.T) hubFixture {
	t.Helper()
	raw, err := os.ReadFile("../../verify/fixtures/all-api-hub-encrypted-backup.json")
	if err != nil {
		t.Fatalf("读加密备份夹具失败: %v", err)
	}
	var f hubFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("解析加密备份夹具失败: %v", err)
	}
	if f.Password == "" || f.Plaintext == "" || len(f.Envelope) == 0 {
		t.Fatal("夹具缺字段，后面的断言会变成空断言")
	}
	return f
}

// TestDecryptHubBackupReadsUpstreamEnvelope 是这套东西的地基：
// 上游加密的备份，我方必须解得开，且解出来的字节要**逐字**等于原文。
func TestDecryptHubBackupReadsUpstreamEnvelope(t *testing.T) {
	f := loadHubFixture(t)
	got, err := DecryptHubBackup(f.Envelope, f.Password)
	if err != nil {
		t.Fatalf("解密上游信封失败: %v", err)
	}
	if string(got) != f.Plaintext {
		t.Fatalf("解出的明文与上游加密前的原文不一致\n got=%q\nwant=%q", got, f.Plaintext)
	}
	// 解出来的必须能直接喂给既有的备份解析器 —— 上游的 WebDAV 载荷与手工导出
	// 同一个形态（BackupFullV2），不该有第二套解析。
	b, err := ParseHubBackup(strings.NewReader(string(got)))
	if err != nil {
		t.Fatalf("解密结果喂给 ParseHubBackup 失败: %v", err)
	}
	if len(b.Accounts.Accounts) != 1 {
		t.Fatalf("解析出的站点数 = %d，want 1", len(b.Accounts.Accounts))
	}
}

// TestDecryptHubBackupWrongPassword 密码错必须报错，不能回一堆乱字节。
func TestDecryptHubBackupWrongPassword(t *testing.T) {
	f := loadHubFixture(t)
	_, err := DecryptHubBackup(f.Envelope, "wrong-password")
	if err == nil {
		t.Fatal("密码错却解密成功了")
	}
	if !strings.Contains(err.Error(), "密码不对") {
		t.Fatalf("错误文案没说清是密码问题: %v", err)
	}
}

// TestDecryptHubBackupTamperedCiphertext GCM 的认证标签必须真的在起作用。
//
// 没有这条的话，"能解开上游信封"只说明参数拼对了，说明不了完整性还在 ——
// 而我们是把解密结果直接建成渠道的，篡改过的备份必须解不开。
func TestDecryptHubBackupTamperedCiphertext(t *testing.T) {
	// 翻掉密文里的一个字节。不在 base64 字符串上做替换：那要求夹具里存在某段
	// 特定文本，夹具一换就变成"没替换到、照样解得开"的空断言。
	f := loadHubFixture(t)
	var env struct {
		CT string `json:"ct"`
	}
	if err := json.Unmarshal(f.Envelope, &env); err != nil {
		t.Fatalf("夹具信封里读不出 ct: %v", err)
	}
	ct := []byte(env.CT)
	// base64 字母表里前后相邻的两个字符，换一个必然解出不同的字节
	flip := byte('A')
	if ct[len(ct)/2] == flip {
		flip = 'B'
	}
	ct[len(ct)/2] = flip
	tampered := mustReplaceJSONString(t, f.Envelope, "ct", string(ct))
	if _, err := DecryptHubBackup(tampered, f.Password); err == nil {
		t.Fatal("密文被改过却解密成功了 —— GCM 认证没起作用")
	}
}

// TestDecryptHubBackupPlainPassthrough 上游的加密是可选的，明文备份要原样过。
func TestDecryptHubBackupPlainPassthrough(t *testing.T) {
	plain := []byte(`{"version":"3.0","accounts":{"accounts":[]}}`)
	got, err := DecryptHubBackup(plain, "")
	if err != nil {
		t.Fatalf("明文备份被当成加密的: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("明文备份被改动了: %q", got)
	}
}

// TestDecryptHubBackupEncryptedWithoutPassword 缺密码要能被上层单独识别。
func TestDecryptHubBackupEncryptedWithoutPassword(t *testing.T) {
	f := loadHubFixture(t)
	_, err := DecryptHubBackup(f.Envelope, "")
	if !errors.Is(err, ErrHubBackupEncrypted) {
		t.Fatalf("缺密码时的错误不是 ErrHubBackupEncrypted: %v", err)
	}
}

// TestDecryptHubBackupRejectsAbsurdIterations 迭代数来自远端文件，必须有上限。
//
// 上游没有这个上限（它跑在浏览器里、密码是用户当场输的）；我们是无人值守的
// 定时任务，一个写着十亿次迭代的文件能让采集进程空转几分钟。
func TestDecryptHubBackupRejectsAbsurdIterations(t *testing.T) {
	f := loadHubFixture(t)
	var env map[string]any
	if err := json.Unmarshal(f.Envelope, &env); err != nil {
		t.Fatalf("解析夹具信封失败: %v", err)
	}
	if _, ok := env["iter"]; !ok {
		t.Fatal("夹具信封里没有 iter 字段，断言会永远绿")
	}
	env["iter"] = 999_999_999
	huge, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecryptHubBackup(huge, f.Password)
	if err == nil {
		t.Fatal("迭代数离谱却照跑不误")
	}
	if !strings.Contains(err.Error(), "迭代数") {
		t.Fatalf("错误文案没指向迭代数: %v", err)
	}
}

// TestHubBackupURL 目录要补出上游的默认路径，直链要原样用。
//
// 路径判据取自上游 ensureFilename。补错了的表现是 404，而 404 在这里会被
// 报成"还没有备份文件"—— 一个看起来正常的状态，于是永远不会有人去查。
func TestHubBackupURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://dav.example/dav", "https://dav.example/dav/all-api-hub-backup/all-api-hub-1-0.json"},
		{"https://dav.example/dav/", "https://dav.example/dav/all-api-hub-backup/all-api-hub-1-0.json"},
		{"https://dav.example/x/my-backup.json", "https://dav.example/x/my-backup.json"},
		{"https://dav.example/x/MY-BACKUP.JSON", "https://dav.example/x/MY-BACKUP.JSON"},
		// 带查询串的直链：判据只看 path，别被 ?name=a.json 骗过去
		{"https://dav.example/dav?token=a.json",
			"https://dav.example/dav/all-api-hub-backup/all-api-hub-1-0.json?token=a.json"},
	}
	for _, c := range cases {
		got, err := hubBackupURL(c.in)
		if err != nil {
			t.Fatalf("hubBackupURL(%q) 报错: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("hubBackupURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "   ", "ftp://dav.example/x", "file:///etc/passwd"} {
		if _, err := hubBackupURL(bad); err == nil {
			t.Errorf("hubBackupURL(%q) 应当被拒绝", bad)
		}
	}
}

// mustReplaceJSONString 把信封里某个字符串字段换成新值，返回改过的 JSON。
func mustReplaceJSONString(t *testing.T, raw json.RawMessage, key, value string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析信封失败: %v", err)
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("重新编码信封失败: %v", err)
	}
	return out
}
