wrk.method = "POST"
wrk.headers["Content-Type"] = "application/json"
wrk.body = '{"batch":[{"message":{"channel":"Test Channel","context":{"app":{"build":"1","name":"AndroidAnalytics","namespace":"com.example.androidanalytics","version":"1.0"},"device":{"id":"70a848aa-ffb2-42ab-9faa-b41594745836","manufacturer":"Google","model":"Android SDK built for x86","name":"generic_x86"},"library":{"name":"com.example.analyticslibrary","version":"1.0"},"locale":"en-US","network":{"carrier":"Android"},"os":{"name":"Android","version":"10"},"screen":{"density":2,"height":1794,"width":1080},"traits":{"anonymous_id":"6250c1b1-e70c-4e1c-aa2a-48309370408f"},"user_agent":"Dalvik/2.1.0 (Linux; U; Android 10; Android SDK built for x86 Build/QPP4.190502.018)"},"event":"Track","integrations":["rudderlabs"],"message_id":"1562325502503","properties":{"query":"blue hotpants"},"timestamp":"2019-07-05 11:18:22+0000","type":"page"}}],"sent_at":"2019-07-05 11:18:23+0000"}'