# SMTP 联调收信后端

这是一个仅用于研发联调的本机 SMTP 收信与只读 HTTP 查询服务。服务不向外转发邮件，也不解析或改写 MIME；`DATA` 中完成点透明解码后的原始字节会直接存入 SQLite。

## 配置

所有配置通过环境变量提供：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SMTP_ADDR` | `127.0.0.1:2525` | SMTP 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `DB_PATH` | `mail.db` | SQLite 数据库路径 |
| `ALLOWED_RECIPIENTS` | 无，必填 | 逗号分隔的允许收件人；域名大小写不敏感，本地部分大小写敏感 |
| `MAX_CONNECTIONS` | `100` | 每种服务各自允许的同时连接数 |
| `READ_TIMEOUT_SECONDS` | `60` | 单次网络读取和 HTTP 处理超时 |
| `MAX_LINE_BYTES` | `4096` | SMTP 命令或邮件单行最大字节数（含 CRLF） |
| `MAX_RECIPIENTS` | `100` | 单封邮件最多不同有效收件人数 |
| `MAX_MAIL_BYTES` | `10485760` | 单封邮件原文最大字节数 |

启动示例：

```sh
ALLOWED_RECIPIENTS='dev@example.com,qa.Name@example.com' \
DB_PATH=/tmp/devmail.db \
SMTP_ADDR=127.0.0.1:2525 \
HTTP_ADDR=127.0.0.1:8080 \
go run .
```

## SMTP 行为

- 支持 `HELO`、`EHLO`、`MAIL FROM`、`RCPT TO`、`DATA`、`RSET`、`NOOP`、`QUIT`。
- 必须先问候，再提交发件人和至少一个配置内收件人，才允许 `DATA`。
- 允许空发件人：`MAIL FROM:<>`。
- 只接受 SMTP 信封收件人；不会读取或推断邮件 `To` 头。
- 重复信封收件人只保存一次。
- 命令必须使用严格 CRLF；支持 TCP 分包和连续命令。
- `DATA` 仅以独占一行的 `.` 加 CRLF 结束，并移除行首转义点。
- 半封邮件断连、行过长或邮件过大时不保存；异常连接会被关闭。
- 消息与收件人关联在同一个 SQLite 事务中提交，提交成功后才返回 `250`。

Python SMTP 客户端示例：

```sh
python3 - <<'PY'
import smtplib
from email.message import EmailMessage

message = EmailMessage()
message['Subject'] = 'local debug'
message['To'] = 'dev@example.com'
message.set_content('hello capture\n')

with smtplib.SMTP('127.0.0.1', 2525) as client:
    client.send_message(message, from_addr='sender@example.org', to_addrs=['dev@example.com'])
PY
```

## HTTP API

所有接口只接受 `GET`，不允许查询字符串。收件人必须在配置中，ID 必须是服务端生成的 32 位小写十六进制字符串。

列出某收件人的邮件元信息：

```sh
curl -i 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages'
```

按 ID 查看实际信封：

```sh
curl -s 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages/<id>'
```

下载未改动的 `.eml` 原文：

```sh
curl --output message.eml 'http://127.0.0.1:8080/api/recipients/dev@example.com/messages/<id>/eml'
```

JSON 元信息包含：`id`、`received_at`、`sender`、`recipients`、`size`。

## 开发验证

```sh
go test ./...
go build ./...
```
