// A scripted OpenAI-compatible model for the workbench checks. Its first
// answer in a conversation asks to run `touch approved.txt`; once the tool's
// result comes back it says "done". Nothing leaves the machine.
import http from 'node:http';

const port = Number(process.argv[2] || 8851);

function sse(res, chunks) {
  res.writeHead(200, {'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache'});
  for (const c of chunks) res.write('data: ' + JSON.stringify(c) + '\n\n');
  res.end('data: [DONE]\n\n');
}

http.createServer((req, res) => {
  let body = '';
  req.on('data', d => { body += d; });
  req.on('end', () => {
    if (req.method !== 'POST') {
      res.writeHead(200, {'Content-Type': 'application/json'});
      return res.end(JSON.stringify({object: 'list', data: [{id: 'workbench-e2e', object: 'model'}]}));
    }
    let msgs = [];
    try { msgs = JSON.parse(body).messages || []; } catch {}
    const answered = msgs.some(m => m.role === 'tool');
    if (answered) {
      return sse(res, [
        {choices: [{index: 0, delta: {role: 'assistant', content: 'done'}}]},
        {choices: [{index: 0, delta: {}, finish_reason: 'stop'}], usage: {prompt_tokens: 10, completion_tokens: 1}},
      ]);
    }
    const args = JSON.stringify({command: 'touch approved.txt', description: 'make a file'});
    sse(res, [
      {choices: [{index: 0, delta: {role: 'assistant', tool_calls: [{index: 0, id: 'call-1', type: 'function',
        function: {name: 'bash', arguments: args}}]}}]},
      {choices: [{index: 0, delta: {}, finish_reason: 'tool_calls'}], usage: {prompt_tokens: 10, completion_tokens: 5}},
    ]);
  });
}).listen(port, '127.0.0.1', () => console.log('fake model on ' + port));
