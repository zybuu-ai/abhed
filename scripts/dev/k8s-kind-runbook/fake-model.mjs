// A scripted OpenAI-compatible model for the kind runbook check. Each answer
// is the next call of a fixed incident flow, chosen by how many tool results
// the conversation holds: read the pods, the deployment and a pod's logs,
// make the runbook's change, make one change the runbook does not list,
// wait for the rollout, then answer. Nothing leaves the machine.
import http from 'node:http';

const port = Number(process.argv[2] || 0);
const context = process.argv[3];
const ns = 'demo';

function sse(res, chunks) {
  res.writeHead(200, {'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache'});
  for (const c of chunks) res.write('data: ' + JSON.stringify(c) + '\n\n');
  res.end('data: [DONE]\n\n');
}

function call(res, n, name, args) {
  sse(res, [
    {choices: [{index: 0, delta: {role: 'assistant', tool_calls: [{index: 0, id: 'call-' + n, type: 'function',
      function: {name, arguments: JSON.stringify(args)}}]}}]},
    {choices: [{index: 0, delta: {}, finish_reason: 'tool_calls'}], usage: {prompt_tokens: 10, completion_tokens: 5}},
  ]);
}

const server = http.createServer((req, res) => {
  let body = '';
  req.on('data', d => { body += d; });
  req.on('end', () => {
    if (req.method !== 'POST') {
      res.writeHead(200, {'Content-Type': 'application/json'});
      return res.end(JSON.stringify({object: 'list', data: [{id: 'kind-runbook', object: 'model'}]}));
    }
    let msgs = [];
    try { msgs = JSON.parse(body).messages || []; } catch {}
    const results = msgs.filter(m => m.role === 'tool');
    const seen = results.map(m => typeof m.content === 'string' ? m.content : JSON.stringify(m.content)).join('\n');
    const pod = (seen.match(/\bweb-[a-z0-9]+-[a-z0-9]+\b/) || [])[0] || 'web';
    const steps = [
      ['k8s_get', {resource: 'pods', namespace: ns, context, selector: 'app=web'}],
      ['k8s_get', {resource: 'deployments', name: 'web', namespace: ns, context}],
      ['k8s_get', {resource: 'logs', name: pod, namespace: ns, context, tail: 5}],
      // The runbook's one step, exactly as listed.
      ['k8s_apply', {action: 'scale', resource: 'deployments', name: 'web', replicas: 2, namespace: ns, context}],
      // Not in the runbook: a headless run must refuse it.
      ['k8s_apply', {action: 'scale', resource: 'deployments', name: 'web', replicas: 5, namespace: ns, context}],
      ['k8s_get', {resource: 'deployments', name: 'web', namespace: ns, context, wait_seconds: 120}],
    ];
    const n = results.length;
    // What each call returned, for run.sh to check that the reads worked.
    if (n > 0) {
      const last = results[n - 1].content;
      console.log('RESULT ' + n + ' ' + JSON.stringify(typeof last === 'string' ? last : JSON.stringify(last)).slice(0, 600));
    }
    if (n < steps.length) return call(res, n + 1, steps[n][0], steps[n][1]);
    sse(res, [
      {choices: [{index: 0, delta: {role: 'assistant', content: 'web in demo was read, then scaled to 2 as the runbook lists.'}}]},
      {choices: [{index: 0, delta: {}, finish_reason: 'stop'}], usage: {prompt_tokens: 10, completion_tokens: 12}},
    ]);
  });
});
server.listen(port, '127.0.0.1', () => console.log('PORT ' + server.address().port));
