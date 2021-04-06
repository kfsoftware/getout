# Alternative to Ngrok
> Documentation is pending...

## Development

### Generate protobuf GO

```bash
protoc -I=$PWD --go_out=$PWD $PWD/messages.proto
```