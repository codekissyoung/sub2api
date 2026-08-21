-- 为 ops_error_logs 增加上游报错抓包列。
-- 运行时开关 openai_error_capture_request_enabled（默认关）打开时，上游错误路径
-- 会落脱敏后的客户端请求 headers 与截断（<=16KB）的 request body，用于排查
-- “什么请求导致上游报错”。正常请求路径零写入。

ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS request_headers TEXT;
ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS request_body TEXT;

COMMENT ON COLUMN ops_error_logs.request_headers IS '上游报错抓包：脱敏后的客户端请求头 JSON（openai_error_capture_request_enabled 开启时写入）';
COMMENT ON COLUMN ops_error_logs.request_body IS '上游报错抓包：截断至 16KB 的客户端请求体（openai_error_capture_request_enabled 开启时写入）';
