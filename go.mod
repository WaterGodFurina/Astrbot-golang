module github.com/WaterGodFurina/Astrbot-golang

go 1.26

require (
	github.com/Baidu-AIP/golang-sdk v1.3.0
	github.com/FloatTech/satori-go v0.0.0-20231020141005-5795eda54d4f
	github.com/WaterGodFurina/Astrbot-go-plugin-sdk v1.2.0
	github.com/WaterGodFurina/astrbot-golang-plugin-python-sdk v0.3.5
	github.com/blusewang/wx v1.3.3
	github.com/bwmarrin/discordgo v0.25.1
	github.com/dobest1024/go-weixin-ilink v0.2.0
	github.com/fogleman/gg v1.3.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/golang/freetype v0.0.0-20170609003504-e2365dfdc4a0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/hashicorp/go-plugin v1.6.2
	github.com/hungpdn/nanovec v0.0.0-20260205215424-956786fcfa80
	github.com/larksuite/oapi-sdk-go/v3 v3.9.10
	github.com/line/line-bot-sdk-go/v8 v8.22.0
	github.com/mark3labs/mcp-go v0.57.0
	github.com/mholt/archives v0.1.5
	github.com/mitchellh/mapstructure v1.5.0
	github.com/pquerna/otp v1.5.0
	github.com/rivo/uniseg v0.4.7
	github.com/slack-go/slack v0.27.0
	github.com/yitsushi/go-misskey v1.1.6
	golang.org/x/crypto v0.55.0
	golang.org/x/image v0.0.0-20190802002840-cff245a6509b
	golang.org/x/mod v0.38.0
	golang.org/x/net v0.58.0
	golang.org/x/sys v0.47.0
	google.golang.org/grpc v1.83.1
	gopkg.in/yaml.v3 v3.0.1
	modernc.org/sqlite v1.56.0
)

require (
	github.com/RomiChan/websocket v1.4.3-0.20220227141055-9b2c6168c9c5 // indirect
	github.com/STARRY-S/zip v0.2.3 // indirect
	github.com/andybalholm/brotli v1.2.0 // indirect
	github.com/bodgit/plumbing v1.3.0 // indirect
	github.com/bodgit/sevenzip v1.6.1 // indirect
	github.com/bodgit/windows v1.0.1 // indirect
	github.com/boombuler/barcode v1.0.1-0.20190219062509-6c824513bacc // indirect
	github.com/dsnet/compress v0.0.2-0.20230904184137-39efe44ab707 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fatih/color v1.7.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/hashicorp/go-hclog v0.14.1 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/hashicorp/yamux v0.1.1 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/klauspost/pgzip v1.2.6 // indirect
	github.com/mattn/go-colorable v0.1.4 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/mikelolasagasti/xz v1.0.1 // indirect
	github.com/minio/minlz v1.0.1 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/nwaples/rardecode/v2 v2.2.0 // indirect
	github.com/oklog/run v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.22 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2 // indirect
	github.com/sirupsen/logrus v1.9.3 // indirect
	github.com/sorairolake/lzip-go v0.3.8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.7.1 // indirect
	github.com/tidwall/gjson v1.17.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/ulikunitz/xz v0.5.16 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/youkale/go-querystruct v1.0.0 // indirect
	go.etcd.io/bbolt v1.4.3 // indirect
	go4.org v0.0.0-20230225012048-214862532bf5 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// nanovec 上游未修复 Windows 编译（flat.go 直接用 unix.Mmap 且无 build tag）；
// 使用 fork（含 Windows mmap 平台封装补丁，内容与上游 956786fcfa80 一致）。
replace github.com/hungpdn/nanovec => github.com/WaterGodFurina/nanovec v1.0.1
