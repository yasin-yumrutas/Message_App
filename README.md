# Relay — Realtime Messaging Engine

Relay, Go ve WebSocket kullanarak aynı oda içindeki istemciler arasında çift yönlü mesaj yayını yapan küçük ölçekli bir gerçek zamanlı iletişim motorudur. Proje ilk olarak bağlantı yaşam döngüsünü öğrenmek için geliştirildi; portfolyo sürümünde oda izolasyonu, güvenli origin politikası, presence olayları ve gözlemlenebilir iki istemcili laboratuvarla yeniden ele alındı.

## Canlı laboratuvar

Tek sayfadaki Ayşe ve Mehmet panelleri, aynı oda hub'ına iki bağımsız WebSocket bağlantısı açar. Panellerden gönderilen mesaj sunucu tarafından yayınlanır ve iki istemcide de görünür. Oda alanı değiştirilerek bağlantılar farklı bir kanal altında yeniden kurulabilir.

Laboratuvar gerçek hesap veya kalıcı mesaj verisi kullanmaz. Sayfadaki örnek konuşmalar yalnızca arayüz bağlamıdır; kullanıcı tarafından gönderilen yeni mesajlar gerçek WebSocket akışından geçer.

## Mimari

```text
Browser client A ─┐
                  ├─ WebSocket endpoint ─ Room hub ─ Broadcast channel
Browser client B ─┘                         │
                                           └─ Presence events
```

- Her oda kendi register, unregister ve broadcast kanallarına sahiptir.
- Her bağlantıda tek reader ve tek writer goroutine çalışır.
- Yavaş istemciler bounded send kuyruğu üzerinden diğer istemcilerden ayrıştırılır.
- Ping/pong, read deadline ve write deadline bağlantı yaşam döngüsünü sınırlar.
- Mesaj, istemci ve oda kimlikleri sunucuda doğrulanır.
- Same-origin kontrolü varsayılandır; ek originler yalnızca `ALLOWED_ORIGINS` ile açılır.
- Arayüz metinleri `textContent` üzerinden çizilir; kullanıcı mesajı HTML olarak işlenmez.

## HTTP yüzeyi

- `GET /` — iki istemcili laboratuvar
- `GET /healthz` — servis sağlık kontrolü
- `GET /ws?room=<room>&id=<client>&name=<name>` — WebSocket upgrade

## Yerel geliştirme

```bash
cd Message_App
go run .
```

Ardından `http://localhost:8080` adresini açın.

## Doğrulama

```bash
cd Message_App
go test ./...
go vet ./...
go build -trimpath .
```

Test paketi güvenli kimlik doğrulamasını, origin politikasını, aynı odadaki broadcast davranışını ve odalar arası mesaj izolasyonunu doğrular.

## Teknolojiler

- Go
- Gorilla WebSocket
- HTTP standard library
- Goroutine ve channel tabanlı concurrency
- Vanilla HTML, CSS ve JavaScript
- Docker

## Kapsam sınırı

Bu proje kalıcı mesaj deposu, kullanıcı hesabı veya yatay ölçekli dağıtık pub/sub katmanı içermez. Üretim ölçeğinde birden fazla servis örneği için Redis/NATS benzeri paylaşılan bir yayın katmanı ve kalıcı veri deposu eklenmelidir.
