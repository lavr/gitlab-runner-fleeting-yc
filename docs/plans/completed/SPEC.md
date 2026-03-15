# ТЗ: Fleeting-плагин для Yandex Cloud

**Репозиторий:** `github.com/lavr/gitlab-runner-fleeting-yc`  
**Go module:** `github.com/lavr/gitlab-runner-fleeting-yc`  
**Бинарник:** `fleeting-plugin-yandexcloud` (имя важно — именно так Fleeting его ищет)

---

## Контекст и референсы

Fleeting — plugin-based библиотека GitLab Runner для абстракции над группами VM. Плагин реализует интерфейс `provider.InstanceGroup`, компилируется в отдельный бинарник, общается с Runner по gRPC через hashicorp/go-plugin.

Агент **обязан прочитать эти источники перед написанием кода** — не угадывать сигнатуры:

```
# Основная библиотека — читать provider/provider.go и plugin/plugin.go
https://gitlab.com/gitlab-org/fleeting/fleeting

# Лучший референс по структуре community-плагина
https://github.com/hetznercloud/fleeting-plugin-hetzner

# Второй референс
https://github.com/cloudscale-ch/fleeting-plugin-cloudscale

# YC Instance Group API (gRPC)
https://cloud.yandex.ru/docs/compute/api-ref/grpc/InstanceGroup/

# Официальный Go SDK YC
https://github.com/yandex-cloud/go-sdk
```

---

## Структура репозитория

```
gitlab-runner-fleeting-yc/
├── main.go
├── plugin.go
├── config.go
├── client.go
├── go.mod
├── go.sum
├── Makefile
├── .golangci.yml
├── .goreleaser.yml
├── README.md
├── .github/
│   └── workflows/
│       ├── test.yml
│       └── release.yml
└── examples/
    └── config.toml
```

---

## Конфигурация (`plugin_config` в config.toml)

```go
type Config struct {
    // Обязательные
    FolderID        string `json:"folder_id"`
    InstanceGroupID string `json:"instance_group_id"`

    // Аутентификация — взаимоисключающие.
    // Если оба пусты — использовать metadata-сервис (SA на VM).
    // Если задан key_file — читать IAM JSON-ключ из файла.
    KeyFile string `json:"key_file,omitempty"`

    // Подключение к VM
    SSHUser         string `json:"ssh_user"`          // default: "ubuntu"
    UseInternalAddr bool   `json:"use_internal_addr"` // default: false
}
```

**Логика auto-detect аутентификации:**

1. Если `key_file` задан и файл существует → `ycsdk.ServiceAccountKey` из JSON
2. Иначе → `ycsdk.InstanceServiceAccount()` (metadata-сервис)
3. Если оба провалились → понятная ошибка с подсказкой

---

## Интерфейс для реализации

Агент читает актуальный `provider.InstanceGroup` из исходников fleeting. Примерный вид:

```go
type InstanceGroup interface {
    Init(ctx context.Context, logger hclog.Logger, settings Settings) (ProviderInfo, error)
    Update(ctx context.Context, fn func(instance string, state State)) error
    Increase(ctx context.Context, delta int) (int, error)
    Decrease(ctx context.Context, instances []string) ([]string, error)
    ConnectInfo(ctx context.Context, instance string) (ConnectInfo, error)
    Shutdown(ctx context.Context) error
}
```

> **Важно:** сигнатуры могут отличаться от приведённых выше. Агент проверяет актуальную версию через `go get` и читает исходный `provider/provider.go` перед написанием кода.

---

## Реализация методов

### `Init`

- Создать YC SDK с auto-detect аутентификации (см. логику выше)
- Вызвать `InstanceGroupService.Get()` — валидация что группа существует и доступна
- Логировать название группы, folder, текущий `target_size`
- Вернуть `ProviderInfo{ID: "yandexcloud", MaxSize: maxSizeFromGroup}`

### `Update`

- Вызвать `InstanceGroupService.ListInstances()` — постраничный обход если инстансов > 100
- Маппинг статусов YC → `provider.State`:

| YC статус | provider.State |
|---|---|
| `RUNNING` | `StateRunning` |
| `STARTING`, `CREATING`, `WARMING_UP`, `OPENING_DISK`, `RECOVERING` | `StateCreating` |
| `STOPPING`, `STOPPED`, `DELETING` | `StateDeleting` |
| всё остальное | `StateUnknown` |

- Вызвать `fn(instanceID, state)` для каждого инстанса

### `Increase`

- Получить текущий `target_size` через `Get()`
- Вычислить новый размер: `min(current + delta, maxSize)`
- Вызвать `UpdateSize(newSize)`
- Вернуть `newSize - current` (фактически добавленных)

### `Decrease`

- Использовать `InstanceGroupService.DeleteInstances()` для удаления конкретных VM по ID — **не** уменьшать `target_size` вслепую
- Обрабатывать частичные ошибки: если часть VM удалена успешно — вернуть успешные, залогировать ошибочные
- Вернуть slice успешно удалённых instance ID

### `ConnectInfo`

- Получить список инстансов через `ListInstances()`
- Найти инстанс по ID
- Извлечь IP:
  - `UseInternalAddr=false` → `networkInterface.primaryV4Address.oneToOneNat.address`
  - `UseInternalAddr=true` → `networkInterface.primaryV4Address.address`
- Вернуть:

```go
ConnectInfo{
    OS:           "linux",
    Protocol:     ConnectInfoProtocolSSH,
    Username:     cfg.SSHUser,
    ExternalAddr: ip + ":22",
}
```

- SSH-ключ **не генерировать в плагине** — пользователь настраивает `connector_config` в `config.toml`. Документировать это явно в README.

### `Shutdown`

- Залогировать завершение
- Ничего не удалять

---

## go.mod

```
module github.com/lavr/gitlab-runner-fleeting-yc

go 1.21
```

Зависимости агент добавляет через `go get` — не хардкодить версии вручную:

```bash
go get gitlab.com/gitlab-org/fleeting/fleeting@latest
go get github.com/yandex-cloud/go-sdk@latest
go get github.com/hashicorp/go-hclog@latest
go get github.com/hashicorp/go-plugin@latest
```

---

## Makefile

```makefile
BINARY := fleeting-plugin-yandexcloud

.PHONY: build test lint install cross clean

build:
	go build -o $(BINARY) .

test:
	go test ./... -v -timeout 60s

lint:
	golangci-lint run ./...

install:
	go install .

cross:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 go build -o dist/$(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o dist/$(BINARY)-linux-arm64 .

clean:
	rm -f $(BINARY)
	rm -rf dist/
```

---

## .goreleaser.yml

```yaml
project_name: fleeting-plugin-yandexcloud

builds:
  - main: .
    binary: fleeting-plugin-yandexcloud
    goos: [linux]
    goarch: [amd64, arm64]
    env:
      - CGO_ENABLED=0
    ldflags:
      - -s -w -X main.version={{.Version}}

archives:
  - format: tar.gz
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"

checksum:
  name_template: "checksums.txt"

release:
  github:
    owner: lavr
    name: gitlab-runner-fleeting-yc
```

---

## GitHub Actions

### `.github/workflows/test.yml`

```yaml
name: test

on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.21'
      - run: go build ./...
      - run: go test ./... -v -timeout 60s
      - run: go vet ./...
```

### `.github/workflows/release.yml`

```yaml
name: release

on:
  push:
    tags: ['v*']

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version: '1.21'
      - uses: goreleaser/goreleaser-action@v5
        with:
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

---

## .golangci.yml

```yaml
linters:
  enable:
    - errcheck
    - govet
    - staticcheck
    - unused
    - gofmt
    - misspell

linters-settings:
  govet:
    check-shadowing: true

issues:
  exclude-rules:
    - path: _test\.go
      linters: [errcheck]
```

---

## README.md — обязательные секции

1. **Overview** — что это, ссылка на Fleeting docs
2. **Compatibility** — версии GitLab Runner и fleeting
3. **Prerequisites**
   - YC Instance Group с `FIXED` scale policy (Fleeting управляет размером сам)
   - Image template: Ubuntu 22.04 с Docker, пользователь `ubuntu` в группе `docker`, SSH-ключ runner manager прописан в metadata
4. **IAM permissions** — минимальный набор прав SA:
   - `compute.instanceGroups.get`
   - `compute.instanceGroups.update`
   - `compute.instanceGroups.delete` (для DeleteInstances)
   - `compute.instances.get`
5. **Installation**
   ```bash
   # Вариант 1: через fleeting install (Runner >= 16.11)
   # В config.toml: plugin = "lavr/gitlab-runner-fleeting-yc:latest"
   gitlab-runner fleeting install

   # Вариант 2: вручную
   curl -L https://github.com/lavr/gitlab-runner-fleeting-yc/releases/latest/download/fleeting-plugin-yandexcloud-linux-amd64.tar.gz | tar xz
   sudo mv fleeting-plugin-yandexcloud /usr/local/bin/
   ```
6. **SSH key setup** — плагин не управляет SSH-ключами; ключ runner manager должен быть прописан в instance template metadata (`ssh-keys: ubuntu:<pubkey>`)
7. **Configuration reference** — все поля `plugin_config` с типами и defaults
8. **Full config.toml example** (содержимое `examples/config.toml`)
9. **Creating Instance Group** — YC CLI пример
10. **Troubleshooting** — типичные ошибки и решения

---

## `examples/config.toml`

```toml
concurrent = 10

[[runners]]
  name     = "yc-docker-autoscaler"
  url      = "https://gitlab.example.com/"
  token    = "glrt-xxxxxxxxxxxx"
  executor = "docker-autoscaler"

  [runners.docker]
    image       = "alpine:latest"
    pull_policy = ["if-not-present"]

  [runners.autoscaler]
    plugin                = "fleeting-plugin-yandexcloud"
    capacity_per_instance = 1
    max_use_count         = 10
    max_instances         = 5

    instance_ready_command = "cloud-init status --wait || test $? -eq 2"

    [runners.autoscaler.plugin_config]
      folder_id         = "b1gxxxxxxxxxxxxxxxxx"
      instance_group_id = "cl1xxxxxxxxxxxxxxxxx"
      # key_file = "/etc/gitlab-runner/yc-key.json"  # если не задан — metadata auto-detect
      ssh_user          = "ubuntu"
      use_internal_addr = false

    [runners.autoscaler.connector_config]
      username          = "ubuntu"
      use_external_addr = true
      # key_path = "/etc/gitlab-runner/runner-ssh-key"

    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time  = "20m"
```

---

## Unit-тесты (обязательно)

Агент пишет тесты с mock YC клиента (через интерфейс, не конкретный тип):

| Тест | Что проверяет |
|---|---|
| `TestInit_ValidGroup` | Успешная инициализация |
| `TestInit_InvalidGroup` | Группа не существует → ошибка |
| `TestUpdate_MapsStates` | Все статусы YC корректно маппятся в provider.State |
| `TestIncrease_RespectsMaxSize` | Не выходит за maxSize |
| `TestDecrease_RemovesSpecificInstances` | Вызывает DeleteInstances с правильными ID |
| `TestConnectInfo_ExternalIP` | Возвращает external IP при UseInternalAddr=false |
| `TestConnectInfo_InternalIP` | Возвращает internal IP при UseInternalAddr=true |
| `TestAuth_KeyFile` | Загружает IAM-ключ из файла |
| `TestAuth_MetadataFallback` | Использует metadata если key_file пуст |

---

## Критерии готовности

- [x] `go build .` → бинарник `fleeting-plugin-yandexcloud` без ошибок
- [x] `go test ./...` → все тесты зелёные
- [x] `go vet ./...` → без замечаний
- [x] Интерфейс `provider.InstanceGroup` реализован полностью с актуальными сигнатурами (прочитан из исходников, не угадан)
- [x] Auto-detect аутентификации: `key_file` → IAM key, пусто → metadata
- [x] Постраничный обход `ListInstances` (pageToken loop)
- [x] `ConnectInfo` возвращает корректный адрес в зависимости от `use_internal_addr`
- [x] `Decrease` использует `DeleteInstances` (удаление конкретных VM, не уменьшение target_size)
- [x] Все публичные типы задокументированы godoc-комментариями
- [x] README содержит все 10 секций
- [x] GitHub Actions: `test.yml` и `release.yml`
- [x] `.goreleaser.yml` собирает `linux/amd64` и `linux/arm64`
- [x] `golangci-lint run` без ошибок

---

## Порядок работы агента

1. Прочитать `provider/provider.go` из репозитория fleeting (актуальные сигнатуры)
2. Прочитать hetzner и cloudscale плагины как референс структуры
3. `go mod init github.com/lavr/gitlab-runner-fleeting-yc`
4. `go get` всех зависимостей
5. Написать код в порядке: `config.go` → `client.go` → `plugin.go` → `main.go`
6. Написать unit-тесты с mock-клиентом
7. Написать README
8. Добавить Makefile, `.goreleaser.yml`, `.golangci.yml`, GitHub Actions
9. Убедиться что `go build` и `go test` проходят без ошибок
