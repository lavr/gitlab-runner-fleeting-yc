# План: Создание Instance Group из YAML-шаблона

## Контекст

Сейчас плагин требует `instance_group_id` — группа инстансов должна быть создана вручную заранее. Это неудобно для автоматизации. Фича позволит плагину самому создавать группу из YAML-шаблона при `Init()` и опционально удалять при `Shutdown()`.

## Ключевое решение: `CreateFromYaml` API

YC SDK имеет метод `CreateFromYaml`, который принимает YAML-строку в формате `yc compute instance-group create --file=spec.yaml`. Это позволяет передать шаблон напрямую в API без парсинга в Go-структуры.

---

## Изменения по файлам

### 1. `config.go` — новые поля конфига

```go
TemplateFile     string `json:"template_file,omitempty"`      // путь к YAML-шаблону
DeleteOnShutdown bool   `json:"delete_on_shutdown,omitempty"`  // удалять группу при остановке
GroupName        string `json:"group_name,omitempty"`          // имя для идемпотентности
```

Валидация:
- `instance_group_id` и `template_file` — взаимоисключающие
- Хотя бы одно из них обязательно
- `group_name` по умолчанию `"fleeting-plugin-yandexcloud"`

### 2. `client.go` — расширение интерфейса

Добавить 3 метода:
- `CreateFromYaml(ctx, *ig.CreateInstanceGroupFromYamlRequest) (*operation.Operation, error)`
- `Delete(ctx, *ig.DeleteInstanceGroupRequest) (*operation.Operation, error)`
- `List(ctx, *ig.ListInstanceGroupsRequest) (*ig.ListInstanceGroupsResponse, error)`

### 3. `plugin.go` — основная логика

**Новое поле:** `createdGroup bool`

**Init() — новый flow:**
1. Если `instance_group_id` задан → текущая логика (Get + validate)
2. Если `template_file` задан:
   - `findExistingGroup()` — ищет группу по имени + лейблу `fleeting-managed-by=fleeting-plugin-yandexcloud`
   - Если найдена → переиспользовать
   - Если нет → читаем YAML, инжектим лейблы, вызываем `CreateFromYaml`, ждём завершения
   - Сохраняем ID, ставим `createdGroup = true`

**Shutdown():**
- Если `createdGroup && DeleteOnShutdown` → вызываем `Delete()`, ждём завершения

**Хелперы:**
- `createGroup(ctx, yamlContent) (string, error)` — создание + ожидание
- `findExistingGroup(ctx) (string, error)` — поиск по `List` + лейблам
- `injectLabels(yamlContent, labels) (string, error)` — инжект лейблов в YAML через `gopkg.in/yaml.v2`

### 4. `plugin_test.go` — тесты

Расширить `mockClient` тремя новыми методами. Новые тест-кейсы:
- Валидация: template_file без instance_group_id, оба заданы, ни одного
- Init: создание группы, переиспользование существующей, ошибки создания
- Shutdown: удаление при `delete_on_shutdown`, без удаления для внешней группы
- `injectLabels`: корректное слияние лейблов

### 5. `examples/instance-group-template.yaml` — пример шаблона

```yaml
name: gitlab-runners
service_account_id: ajeXXXXXXXXXXXXXXXXX
instance_template:
  platform_id: standard-v3
  resources_spec:
    cores: 2
    memory: 4294967296
    core_fraction: 100
  boot_disk_spec:
    disk_spec:
      size: 32212254720
      type_id: network-ssd
      image_id: fd8XXXXXXXXXXXXXXXXX
  network_interface_specs:
    - network_id: enpXXXXXXXXXXXXXXXXX
      subnet_ids: [e9bXXXXXXXXXXXXXXXXX]
      primary_v4_address_spec:
        one_to_one_nat_spec:
          ip_version: IPV4
scale_policy:
  fixed_scale:
    size: 0
deploy_policy:
  max_unavailable: 1
  max_expansion: 1
allocation_policy:
  zones:
    - zone_id: ru-central1-a
```

### 6. `examples/config.toml` — дополнить пример

Добавить закомментированный блок с `template_file` режимом.

### 7. `README.md` — документация

---

## Идемпотентность

При рестарте плагин ищет ранее созданную группу через `List` API по:
- Имени = `group_name`
- Лейблу `fleeting-managed-by=fleeting-plugin-yandexcloud`

Если найдена одна — переиспользует. Если несколько — ошибка с рекомендацией указать `instance_group_id`.

## Порядок реализации

### Task 1: config.go — новые поля конфига
- [x] Добавить поля TemplateFile, DeleteOnShutdown, GroupName в Config
- [x] Обновить validate(): instance_group_id и template_file взаимоисключающие, хотя бы одно обязательно
- [x] GroupName по умолчанию "fleeting-plugin-yandexcloud"
- [x] Тесты валидации для новых полей

### Task 2: client.go — расширение интерфейса
- [x] Добавить CreateFromYaml в InstanceGroupClient и sdkClient
- [x] Добавить Delete в InstanceGroupClient и sdkClient
- [x] Добавить List в InstanceGroupClient и sdkClient

### Task 3: plugin.go — основная логика (Init flow)
- [x] Добавить поле createdGroup bool в InstanceGroup
- [x] Реализовать injectLabels(yamlContent, labels) (string, error)
- [x] Реализовать findExistingGroup(ctx) (string, error)
- [x] Реализовать createGroup(ctx, yamlContent) (string, error)
- [x] Обновить Init(): новый flow для template_file
- [x] Обновить Shutdown(): удаление группы при createdGroup && DeleteOnShutdown

### Task 4: plugin_test.go — тесты
- [x] Расширить mockClient тремя новыми методами
- [x] Тесты Init: создание группы, переиспользование существующей, ошибки создания
- [x] Тесты Shutdown: удаление при delete_on_shutdown, без удаления для внешней группы
- [x] Тесты injectLabels: корректное слияние лейблов

### Task 5: примеры и документация
- [x] examples/instance-group-template.yaml — пример шаблона
- [x] examples/config.toml — дополнить закомментированным блоком template_file
- [x] README.md — документация новой фичи

## Верификация

- `go test ./...`
- `go vet ./...`
- `golangci-lint run`
- Ручное тестирование: запуск с `template_file` в реальном YC-окружении
