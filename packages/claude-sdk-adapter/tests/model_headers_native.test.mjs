import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

// The pinned SDK runs its real model/tool loop against synthetic HTTP responses.
// Headers are non-sensitive metadata; native tools may read them.
// This qualifies transport against synthetic responses, not live provider acceptance.
test('native activity headers survive retry, tool continuation, reused messages and resume; tools may read them', { skip: process.env.OAC_TEST_CLAUDE_HEADERS_NATIVE !== '1', timeout: 120000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), 'oac-header-native-'));
  let attempts = 0;
  let toolSent = false;
  const received = [];
  let toolEnvironmentExposed = false;
  const server = createServer(async (req, res) => {
    if (!req.url.includes('/messages')) { res.writeHead(200, {'content-type':'application/json'}); res.end('{}'); return; }
    const chunks = []; for await (const chunk of req) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString());
    for (const message of body.messages ?? []) for (const content of Array.isArray(message.content) ? message.content : []) {
      if (content.type === 'tool_result' && JSON.stringify(content.content).includes('HEADER-ENV-EXPOSED')) toolEnvironmentExposed = true;
    }
    received.push(req.headers);
    attempts++;
    if (attempts === 1) { res.writeHead(500, {'content-type':'application/json'}); res.end(JSON.stringify({type:'error',error:{type:'api_error',message:'synthetic retry'}})); return; }
    const useTool = !toolSent; toolSent = true;
    const content = useTool ? {type:'tool_use',id:'tool_fixture',name:'Bash',input:{command:'if test -n "${ANTHROPIC_CUSTOM_HEADERS+x}"; then printf HEADER-ENV-EXPOSED; else printf HEADER-ENV-PROTECTED; fi',description:'Read fixture'}} : {type:'text',text:'HEADER-OK'};
    const message = {id:'msg_fixture_'+attempts,type:'message',role:'assistant',model:body.model,content:[content],stop_reason:useTool?'tool_use':'end_turn',stop_sequence:null,usage:{input_tokens:10,output_tokens:10}};
    if (!body.stream) { res.writeHead(200, {'content-type':'application/json'}); res.end(JSON.stringify(message)); return; }
    res.writeHead(200, {'content-type':'text/event-stream'});
    const send = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({type,...data})}\n\n`);
    send('message_start',{message:{...message,content:[],stop_reason:null}});
    send('content_block_start',{index:0,content_block:useTool?{...content,input:{}}:{type:'text',text:''}});
    send('content_block_delta',{index:0,delta:useTool?{type:'input_json_delta',partial_json:JSON.stringify(content.input)}:{type:'text_delta',text:content.text}});
    send('content_block_stop',{index:0}); send('message_delta',{delta:{stop_reason:message.stop_reason,stop_sequence:null},usage:{output_tokens:10}}); send('message_stop',{}); res.end();
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  const endpoint = `http://127.0.0.1:${server.address().port}`;
  const env = {...process.env, HOME:root, CLAUDE_CONFIG_DIR:join(root,'state'), ANTHROPIC_BASE_URL:endpoint,ANTHROPIC_AUTH_TOKEN:'fixture-only',ANTHROPIC_CUSTOM_HEADERS:'X-Sandbase-Benefit: daily-free\nX-Literal: ${unchanged}:value\nX-Empty: ',CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC:'1',DISABLE_TELEMETRY:'1',DISABLE_ERROR_REPORTING:'1'};
  delete env.ANTHROPIC_API_KEY; delete env.CLAUDE_CODE_OAUTH_TOKEN;
  const { query } = await import('@anthropic-ai/claude-agent-sdk');
  let session;
  try {
    for (let turn=0;turn<2;turn++) {
      let release; const ready = new Promise(resolve => { release = resolve; });
      async function* messages() { yield {type:'user',message:{role:'user',content:'Reply HEADER-OK.'},parent_tool_use_id:null}; await ready; yield {type:'user',message:{role:'user',content:'Reply HEADER-OK again.'},parent_tool_use_id:null}; }
      const stream = query({prompt:turn===0?messages():'Reply HEADER-OK.',options:{cwd:root,env,model:'claude-sonnet-4-5',tools:['Bash'],allowedTools:['Bash'],permissionMode:'default',settingSources:[],maxTurns:4,...(session?{resume:session}:{})}});
      let result;
      for await (const event of stream) { if(event.session_id)session=event.session_id; if(event.type==='result'){result=event;release();} }
      assert.equal(result?.is_error,false);
    }
    assert.equal(toolEnvironmentExposed, true, 'native tool visibility must be explicit in this qualification');
    assert.ok(attempts>=5, 'retry + tool continuation + reused query + resumed turn were not captured');
    for (const headers of received) {
      assert.equal(headers['x-sandbase-benefit'],'daily-free');
      assert.equal(headers['x-literal'],'${unchanged}:value');
      assert.equal(headers['x-empty'],'');
    }
  } finally { server.closeAllConnections(); await new Promise(resolve=>server.close(resolve)); await rm(root,{recursive:true,force:true}); }
});
