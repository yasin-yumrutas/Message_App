# Relay — Realtime Messaging Engine

Relay, Go ve Gorilla WebSocket ile geliştirilmiş capability-secured, oda tabanlı bir gerçek zamanlı mesajlaşma motorudur. Kullanıcılar public veya private oda oluşturur, oda kimliğiyle katılır ve aynı oda içindeki gerçek tarayıcı istemcileriyle iletişime devam eder.

Canlı laboratuvar: <https://relay-messaging-engine.onrender.com/>

## Neden basit bir WebSocket demosu değil?

WebSocket endpoint'i kullanıcıdan doğrudan oda veya istemci kimliği kabul etmez. Bağlantı üç aşamalı bir yetkilendirme akışı kullanır:

```text
POST /api/rooms
  └─ server-generated room ID
  └─ private ise 192-bit capability secret (yalnızca bir kez döner)

POST /api/tickets
  └─ room + display name + capability doğrulaması
  └─ server-generated client ID
  └─ 30 saniyelik tek kullanımlık WebSocket bileti

GET /ws?ticket=...
  └─ origin policy
  └─ atomic ticket consumption
  └─ room event loop registration
```

Private erişim anahtarının kendisi sunucuda tutulmaz. SHA-256 özeti saklanır ve doğrulama constant-time karşılaştırmayla yapılır. Davet bağlantısı anahtarı URL fragment içinde taşır; fragment HTTP isteğine ve sunucu loglarına gönderilmez.

## Backend mimarisi

- Her oda tek bir event loop içinde `register`, `unregister` ve `broadcast` kanallarını işler.
- Her WebSocket bağlantısı bir reader ve bir writer goroutine kullanır.
- İstemci kimlikleri sunucu tarafından kriptografik rastgelelikle üretilir.
- Room event'leri monoton artan sequence numarası taşır.
- İstemci başına bounded send queue, yavaş tüketicinin oda yayınını bloke etmesini engeller.
- Ping/pong ile read deadline ve write deadline bağlantı yaşam döngüsünü sınırlar.
- Mesajlar typed command/event protokolü kullanır (`message.send` → `message.created`).
- Oda kapasitesi 50 istemci; global oda kapasitesi 500'dür.
- İstemci başına mesaj limiti 10 saniyede 20 komuttur; tekrarlanan ihlal bağlantıyı sonlandırır.
- Frame boyutu 4 KB, mesaj uzunluğu 1.000 rune ile sınırlıdır.
- Boş odalar 30 dakika sonra janitor tarafından bellekten kaldırılır.
- Oda oluşturma ve bilet endpoint'lerinde IP tabanlı fixed-window rate limit bulunur.
- Same-origin varsayılandır; ek originler yalnızca `ALLOWED_ORIGINS` üzerinden açılır.

## HTTP ve WebSocket yüzeyi

### `POST /api/rooms`

```json
{
  "name": "Backend Review",
  "visibility": "private"
}
```

Private odada yanıt `room` nesnesine ek olarak yalnızca bir kez gösterilen `access_key` döndürür.

### `GET /api/rooms/{room_id}`

Oda adı, public/private durumu, aktif katılımcı sayısı, kapasite ve oluşturulma zamanını döndürür. Secret veya üye verisi açığa çıkarmaz.

### `POST /api/tickets`

```json
{
  "room_id": "rm_...",
  "name": "Yasin",
  "access_key": "private odada zorunlu"
}
```

Başarılı yanıt 30 saniyelik, tek kullanımlık bilet ve server-generated client ID döndürür.

### `GET /ws?ticket={ticket}`

WebSocket upgrade öncesinde origin ve bilet doğrulanır. Aynı bilet ikinci kez kullanılamaz.

### Operasyon endpoint'leri

- `GET /healthz` — durum, oda/bağlantı sayısı ve uptime
- `GET /metrics` — Prometheus text formatında room, connection, message, rejection, slow-consumer ve ticket sayaçları

## Event protokolü

İstemciden sunucuya:

```json
{"type":"message.send","text":"Merhaba"}
```

Sunucudan istemciye:

- `presence.join`
- `presence.leave`
- `message.created`
- `error`

Her event `id`, `room_id`, `sequence` ve `sent_at` alanlarını taşır. Presence event'leri güncel katılımcı listesini içerir.

## Yerel geliştirme

```bash
cd Message_App
go run .
```

Ardından <http://localhost:8080> adresini açın. Private bir oda oluşturup davet bağlantısını ikinci tarayıcıda açarak iki gerçek istemciyle test edebilirsiniz.

## Doğrulama

```bash
cd Message_App
go test -count=1 ./...
go vet ./...
go build -trimpath .
```

Test paketi şunları doğrular:

- private oda erişim anahtarı ve yanlış anahtar reddi,
- tek kullanımlık bilet replay koruması,
- server-generated istemci kimliği,
- aynı oda broadcast davranışı,
- odalar arası mesaj izolasyonu,
- oda kapasitesi ve boş oda expiry,
- origin allowlist,
- operasyon metrikleri.

## Güvenlik ve kapsam sınırı

Bu sürüm hesap tabanlı kimlik doğrulama yerine paylaşılabilir capability secret kullanır. Mesajlar bellektedir; servis yeniden başladığında odalar ve mesajlar silinir. Tek instance deployment bilinçli bir demo sınırıdır.

Üretim ölçeğinde sonraki adımlar:

- hesap/organizasyon tabanlı OAuth veya passkey kimliği,
- PostgreSQL üzerinde oda üyeliği ve mesaj geçmişi,
- Redis/NATS tabanlı çok instance pub/sub ve presence,
- capability rotation/revocation,
- reverse proxy seviyesinde gerçek istemci IP rate limiting,
- OpenTelemetry trace ve merkezi log/metric pipeline.
