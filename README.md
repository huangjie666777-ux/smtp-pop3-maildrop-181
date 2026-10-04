# SMTP 联调收信后端

这是一个仅用于研发联调的本机 SMTP 收信、POP3 取信与只读 HTTP 归档查询服务。服务不向外转发邮件，也不解析或改写 MIME；`DATA` 中完成点透明解码后的原始字节会直接存入 SQLite。

## 配置

所有配置通过环境变量提供：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SMTP_ADDR` | `127.0.0.1:2525` | SMTP 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `HTTP_ADDR` | `127.0.0.1:8080` | HTTP 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `POP3_ENABLED` | `false` | 是否启用 POP3；缺省时保持只启动 SMTP 与 HTTP |
| `POP3_ADDR` | `127.0.0.1:1100` | POP3 监听地址，只允许 `127.0.0.1` 或 `::1` |
| `POP3_PASSWORD` | 无 | POP3 启用时必填；所有允许收件地址共用此联调密码 |
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
POP3_ENABLED=true \
POP3_ADDR=127.0.0.1:1100 \
POP3_PASSWORD=dev-password \
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

## POP3 行为

- 不提供 TLS 或 APOP，仅适合本机联调；认证前不允许访问邮件。
- `USER` 使用 `ALLOWED_RECIPIENTS` 中的完整收件地址，复用域名大小写规则；`PASS` 必须等于 `POP3_PASSWORD`。
- 支持 `STAT`、`LIST`、`UIDL`、`RETR`、`DELE`、`RSET`、`NOOP`、`QUIT`。
- 认证成功时按接收时刻和 ID 升序冻结邮箱快照并从 1 编号；登录期间同一邮箱的第二个会话立即被拒绝。
- POP3 会话不阻塞其他邮箱，也不阻塞 SMTP；登录后到达的新邮件下次登录可见。
- `UIDL` 使用数据库中的稳定 32 位消息 ID，跨会话和重启稳定。
- `DELE` 只在会话内标记，后续 `STAT`、`LIST`、`UIDL` 和 `RETR` 排除该编号但不重编号；`RSET` 撤销标记。
- 只有认证后收到并正常处理 `QUIT` 时，才在同一个 SQLite 事务中删除该收件人的关联副本；断连、超时或删除失败都不会删除。
- 同封邮件的其他收件人仍保留各自副本；当最后一个收件人副本删除时才删除消息正文。

命令行示例：

```sh
python3 - <<'PY'
import poplib

client = poplib.POP3('127.0.0.1', 1100)
client.user('dev@example.com')
client.pass_('dev-password')
print(client.stat())
print(client.list())
print(client.uidl())
print(b'\r\n'.join(client.retr(1)[1]).decode())
# client.dele(1)  # 确认后取消注释，QUIT 时删除该收件人的副本
client.quit()
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
