# Third-Party Licenses — Slagworks

This file inventories the licenses of third-party software distributed with or
depended upon by slagworks. It is generated from the vendored module tree
(`vendor/`) plus the prebuilt boot binaries in `assets/`. Slagworks' own code is
licensed under MIT OR Apache-2.0 (see `LICENSE.txt`).

**Summary:** all Go dependencies are under permissive licenses (MIT, Apache-2.0,
BSD, ISC). Three modules are under MPL-2.0 (file-level weak copyleft, used
unmodified — no source-disclosure obligation on slagworks' own code). One module
includes a CC-BY-SA-4.0 specification *document* (not code). There is **no
GPL/LGPL** in the Go dependency tree.

The boot-time iPXE binaries in `assets/ipxe/` are GPL-licensed and are addressed
separately at the end of this file.

Counts (vendored Go modules):

| License | Modules |
|---------|--------:|
| MIT | 68 |
| Apache-2.0 | 61 |
| BSD (2/3-Clause) | 36 |
| MPL-2.0 | 3 |
| ISC | 1 |
| CC-BY-SA-4.0 (doc only) | 1 |

---

## Go module licenses

#### Apache-2.0
- github.com/aws/aws-sdk-go-v2
- github.com/aws/aws-sdk-go-v2/config
- github.com/aws/aws-sdk-go-v2/credentials
- github.com/aws/aws-sdk-go-v2/feature/ec2/imds
- github.com/aws/aws-sdk-go-v2/internal/configsources
- github.com/aws/aws-sdk-go-v2/internal/endpoints/v2
- github.com/aws/aws-sdk-go-v2/internal/v4a
- github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding
- github.com/aws/aws-sdk-go-v2/service/internal/presigned-url
- github.com/aws/aws-sdk-go-v2/service/secretsmanager
- github.com/aws/aws-sdk-go-v2/service/signin
- github.com/aws/aws-sdk-go-v2/service/sso
- github.com/aws/aws-sdk-go-v2/service/ssooidc
- github.com/aws/aws-sdk-go-v2/service/sts
- github.com/aws/smithy-go
- github.com/bmc-toolbox/bmclib/v2
- github.com/bmc-toolbox/common
- github.com/containerd/continuity
- github.com/containerd/errdefs
- github.com/containerd/errdefs/pkg
- github.com/coreos/go-oidc/v3
- github.com/distribution/reference
- github.com/docker/cli
- github.com/docker/go-connections
- github.com/docker/go-units
- github.com/go-jose/go-jose/v4
- github.com/go-logr/logr
- github.com/go-logr/stdr
- github.com/google/shlex
- github.com/inconshreveable/mousetrap
- github.com/jacobweinstock/iamt/internal/wsman
- github.com/jacobweinstock/registrar
- github.com/kylelemons/godebug
- github.com/moby/docker-image-spec
- github.com/moby/moby/api
- github.com/moby/moby/client
- github.com/moby/sys/user
- github.com/moby/term
- github.com/opencontainers/go-digest
- github.com/opencontainers/image-spec
- github.com/opencontainers/runc
- github.com/ory/dockertest/v3
- github.com/prometheus/client_golang
- github.com/prometheus/client_model
- github.com/prometheus/common
- github.com/prometheus/procfs
- github.com/sethvargo/go-retry
- github.com/spf13/cobra
- github.com/VictorLowther/simplexml
- github.com/VictorLowther/soap
- github.com/vishvananda/netns
- github.com/xeipuuv/gojsonpointer
- github.com/xeipuuv/gojsonreference
- github.com/xeipuuv/gojsonschema
- go.opentelemetry.io/auto/sdk
- go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp
- go.opentelemetry.io/otel
- go.opentelemetry.io/otel/metric
- go.opentelemetry.io/otel/trace
- go.yaml.in/yaml/v2
- gopkg.in/yaml.v2

#### MIT
- github.com/aymanbagabas/go-osc52/v2
- github.com/Azure/go-ansiterm
- github.com/beorn7/perks
- github.com/catppuccin/go
- github.com/cenkalti/backoff/v4
- github.com/cespare/xxhash/v2
- github.com/charmbracelet/bubbles
- github.com/charmbracelet/bubbletea
- github.com/charmbracelet/colorprofile
- github.com/charmbracelet/huh
- github.com/charmbracelet/lipgloss
- github.com/charmbracelet/ultraviolet
- github.com/charmbracelet/x/ansi
- github.com/charmbracelet/x/cellbuf
- github.com/charmbracelet/x/exp/ordered
- github.com/charmbracelet/x/exp/strings
- github.com/charmbracelet/x/term
- github.com/charmbracelet/x/termios
- github.com/charmbracelet/x/vt
- github.com/charmbracelet/x/windows
- github.com/clipperhouse/displaywidth
- github.com/clipperhouse/uax29/v2
- github.com/creack/pty
- github.com/dustin/go-humanize
- github.com/erikgeiser/coninput
- github.com/felixge/httpsnoop
- github.com/glebarez/sqlite/License
- github.com/go-viper/mapstructure/v2
- github.com/jackc/pgpassfile
- github.com/jackc/pgservicefile
- github.com/jackc/pgx/v5
- github.com/jackc/puddle/v2
- github.com/jacobweinstock/iamt
- github.com/Jeffail/gabs/v2
- github.com/jinzhu/inflection
- github.com/jinzhu/now/License
- github.com/josharian/native/license
- github.com/labstack/echo/v4
- github.com/labstack/gommon
- github.com/lucasb-eyer/go-colorful
- github.com/mattn/go-colorable
- github.com/mattn/go-isatty
- github.com/mattn/go-runewidth
- github.com/mdlayher/arp
- github.com/mdlayher/ethernet
- github.com/mdlayher/packet
- github.com/mdlayher/socket
- github.com/mfridman/interpolate
- github.com/Microsoft/go-winio
- github.com/mitchellh/hashstructure/v2
- github.com/muesli/ansi
- github.com/muesli/cancelreader
- github.com/muesli/termenv
- github.com/ncruces/go-strftime
- github.com/pin/tftp/v3
- github.com/pressly/goose/v3
- github.com/rivo/uniseg
- github.com/satori/go.uuid
- github.com/sirupsen/logrus
- github.com/valyala/bytebufferpool
- github.com/valyala/fasttemplate
- github.com/xo/terminfo
- go.uber.org/multierr
- go.yaml.in/yaml/v2
- go.yaml.in/yaml/v3
- gopkg.in/yaml.v2
- gorm.io/driver/postgres/License
- gorm.io/gorm

#### BSD
- dario.cat/mergo
- github.com/atotto/clipboard
- github.com/aws/aws-sdk-go-v2/internal/sync/singleflight
- github.com/aws/smithy-go/internal/sync/singleflight
- github.com/cyphar/filepath-securejoin
- github.com/ghodss/yaml
- github.com/glebarez/go-sqlite
- github.com/go-jose/go-jose/v4/json
- github.com/gofrs/flock
- github.com/google/uuid
- github.com/insomniacslk/dhcp
- github.com/munnerz/goautoneg
- github.com/Nvveen/Gotty
- github.com/ory/dockertest/v3/docker
- github.com/pierrec/lz4/v4
- github.com/pkg/errors
- github.com/prometheus/client_golang/internal/github.com/golang/gddo
- github.com/remyoudompheng/bigfft
- github.com/spf13/pflag
- github.com/stmcginnis/gofish
- github.com/u-root/uio
- golang.org/x/crypto
- golang.org/x/exp
- golang.org/x/net
- golang.org/x/oauth2
- golang.org/x/sync
- golang.org/x/sys
- golang.org/x/term
- golang.org/x/text
- golang.org/x/time
- google.golang.org/protobuf
- modernc.org/libc
- modernc.org/mathutil
- modernc.org/memory
- modernc.org/memory
- modernc.org/sqlite

#### ISC
- github.com/coder/websocket

#### MPL-2.0
- github.com/cyphar/filepath-securejoin
- github.com/hashicorp/errwrap
- github.com/hashicorp/go-multierror

#### CC-BY-SA-4.0
- github.com/opencontainers/go-digest

---

## MPL-2.0 modules (file-level copyleft)

The following are licensed under the Mozilla Public License 2.0. MPL-2.0 is
file-level copyleft: only modifications to the licensed files themselves must be
shared. Slagworks uses these unmodified as libraries, so there is no
source-disclosure obligation on slagworks' own code, and they are freely
redistributable.

- github.com/hashicorp/errwrap
- github.com/hashicorp/go-multierror
- github.com/cyphar/filepath-securejoin (dual BSD-3-Clause + MPL-2.0)

## CC-BY-SA-4.0 (documentation only)

- github.com/opencontainers/go-digest — the module's Go code is Apache-2.0; its
  specification document is licensed CC-BY-SA-4.0. This applies to the doc file,
  not to the code slagworks links against.

---

## Prebuilt binaries in assets/

### iPXE (assets/ipxe/snponly.efi, assets/ipxe/undionly.kpxe)

Pinned to upstream iPXE **v2.0.0** (see `assets/ipxe/UPSTREAM-REF`), built from
source via the `build-ipxe.yaml` workflow.

iPXE is licensed under **GPL-2.0** (with the standard iPXE permissive exception
for certain configurations). These binaries are separate executables invoked at
boot time; they are not linked into any slagworks Go binary, so they are
GPL-compatible with slagworks' MIT/Apache-2.0 license.

**GPL source-availability:** the corresponding source is upstream iPXE at the
tag recorded in `UPSTREAM-REF`. Source: https://github.com/ipxe/ipxe (tag
v2.0.0). Our build configuration is in `.github/workflows/build-ipxe.yaml`.

---

*Regenerate this file by scanning `vendor/**/LICENSE*` after any dependency
change. There is no committed generator script; the listing above was produced
by classifying each vendored license file.*
