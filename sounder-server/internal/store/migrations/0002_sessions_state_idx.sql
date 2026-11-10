-- 会话删除补偿每分钟按 state 扫描一次；会话量大时避免全表扫描。
CREATE INDEX IF NOT EXISTS sessions_state_idx ON sessions (state);
