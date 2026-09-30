'use strict';
// Go supplies lexical AST/template-verified nodes from either the pinned source
// fixture or the actual compiled management HTML. Only provider transport and
// display parsers are mocked; selector and index resolvers execute unchanged.
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const {createHash} = require('node:crypto');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const settle = () => new Promise(resolve => setImmediate(resolve));
const plain = value => JSON.parse(JSON.stringify(value));
const digest = value => createHash('sha256').update(value).digest('hex');
const proof = (selector, value) => ({v: 1, selector_hashes: {[selector]: digest(value)}});
const providers = ['antigravity','claude','codex','devin','kimi','meta','xai'];
const states = {codex:{status:'success',windows:[]},claude:{status:'success',windows:[]},devin:{status:'success',windows:[]},kimi:{status:'success',rows:[]},antigravity:{status:'success',groups:[]},xai:{status:'success',billing:{usagePercent:10}}};
const who = (provider='codex', name=provider+'.json', extra={}) => ({provider,key:provider==='devin'?name+'\0i':name,account:'acct-'+provider,account_kind:'email',auth_index:'i',...(provider==='codex'?{manual_source_proof:proof('account_id','B')}:provider==='antigravity'?{manual_source_proof:proof('project_id','B')}:provider==='xai'?{manual_source_proof:proof('user_id','B')}:{ }),...extra});
const file = (provider='codex', extra={}) => ({name:provider+'.json',auth_index:'i',id_token:{chatgpt_account_id:'B'},project_id:'B',sub:'B',...extra});
function store(initial) {let state=initial;const listeners=[];return {getState:()=>state,subscribe(fn){listeners.push(fn);},setState(patch){const previous=state;patch=typeof patch==='function'?patch(state):patch;if(patch===state)return;state={...state,...patch};for(const fn of listeners)fn(state,previous);}};}
function setup() {
  const timers=new Map(),events=new Map(),calls=[];let timer=0;
  const h={bindings:providers.map(p=>who(p)),calls,providerCalls:[],downloads:[],downloadBody:JSON.stringify({installed:{project_id:'B'}}),billingFailure:false};
  const quota=store({cacheGeneration:0,fileGenerations:{},...Object.fromEntries(providers.map(p=>[p+'Quota',{}]))});
  const window={atob,dispatchEvent(){},addEventListener(name,fn){events.set(name,fn);}};
  const context=vm.createContext({window,URL,AbortController,DOMException,TextEncoder,atob,console:{warn(){}},location:{origin:'http://localhost',href:'http://localhost/management.html'},document:{hidden:false},navigator:{onLine:true},CustomEvent:class{},setTimeout(fn,ms){const id=++timer;timers.set(id,{fn,ms});return id;},clearTimeout(id){timers.delete(id);},fetch:async(url,options)=>{calls.push({url,options});return {ok:true,status:200,json:async()=>url.endsWith('/identities')?{bindings:structuredClone(h.bindings)}:url.endsWith('/cache')?{entries:[]}:{snapshots:[]}};}});
  vm.runInContext(input.bridge,context);const api=window.CPAQuotaPersistence;
  const wrapSource=api.wrapQuotaSource;h.sourceReturns=[];
  api.wrapQuotaSource=(original,project)=>wrapSource(function(...args){const result=Reflect.apply(original,this,args);h.sourceReturns.push(result);return result;},project);
  const update=api.wrapUpdater((next,previous)=>typeof next==='function'?next(previous):next);
  h.update=(provider,next)=>quota.setState(state=>({[provider+'Quota']:update(next,state[provider+'Quota'])}));
  h.capture=api.wrapCapture(name=>{const {cacheGeneration,fileGenerations}=quota.getState();return {cacheGeneration,fileGenerations,name};});
  h.commit=api.wrapCommit((gen,fn,name=gen.name)=>{const current=quota.getState();if(current.cacheGeneration!==gen.cacheGeneration)return false;if(name!==undefined){if((current.fileGenerations[name]??0)!==(gen.fileGenerations[name]??0))return false;}else if(current.fileGenerations!==gen.fileGenerations)return false;fn();return true;});
  const clear=[];quota.setState({clearQuotaCache(names){clear.push(names);quota.setState(state=>{if(names){if(!names.length)return state;const set=new Set(names),fileGenerations={...state.fileGenerations};for(const n of names)fileGenerations[n]=(fileGenerations[n]??0)+1;return {fileGenerations,...Object.fromEntries(providers.map(p=>[p+'Quota',Object.fromEntries(Object.entries(state[p+'Quota']).filter(([key])=>!set.has(key.split('\0')[0])))]))};}return {cacheGeneration:state.cacheGeneration+1,fileGenerations:{},...Object.fromEntries(providers.map(p=>[p+'Quota',{}]))};});}});
  const set=(name,value)=>{const symbol=input.names[name];if(!symbol)throw Error('Missing slot '+name);context[symbol]=value;};
  const ref=(name,value)=>set('ref'+Buffer.from(name).toString('hex'),value);
  // Neutral defaults are only for non-selector view/config references.
  for(const [name,symbol] of Object.entries(input.names))if(name.startsWith('ref'))context[symbol]=()=>null;
  set('quota',quota);
  ref('Lp',{request:async payload=>{h.providerCalls.push(plain(payload));if(h.holdProvider)return h.holdProvider(payload);return {statusCode:h.billingFailure&&['weekly','monthly'].includes(payload.url)?500:200,body:{config:{usagePercent:10},groups:[{id:'group',buckets:[]}],windows:[]}};}});
  ref('By',{downloadText(name){h.downloads.push(name);return h.downloadPromise||Promise.resolve(h.downloadBody);}});
  ref('Eg','usage');ref('kg','consume');ref('_k',()=> 'request-id');ref('f_',()=>null);ref('m_',()=>null);ref('mk',async()=>null);
  ref('r_',value=>value);ref('Qg',value=>value);ref('Q_',()=>({availableCount:null,applicableAvailableCount:null}));ref('hk',async()=>({credits:[],availableCount:0,applicableAvailableCount:0,error:''}));ref('dk',()=>[]);
  ref('D_',(message,status)=>Object.assign(new Error(message),{status}));ref('Ip',()=> 'provider failed');
  ref('tee','version');ref('nee','client');ref('bg',{osType:'os',arch:'arch'});ref('yg',['ag']);ref('gv',{get:async()=>null});ref('hO',value=>value);ref('t_',value=>value);ref('P_',value=>value.groups);ref('mO',()=>null);ref('O_',error=>error.status);
  ref('Ng','weekly');ref('Pg','monthly');ref('Fg','profile');ref('Ig','chat');ref('Nk',15000);ref('Lg','model');ref('a_',value=>value);ref('aee',value=>value);ref('oee',(a,b)=>a||b);ref('lv',()=>({mode:'paid',healthStatus:'ok'}));set('paid',item=>item.paid===true);
  ref('Cg','claude');ref('Sg','claude-plan');ref('n_',value=>value);ref('RO',()=>[]);ref('BO',value=>value);ref('VO',()=>null);
  ref('jg','kimi');ref('i_',value=>value);ref('W_',()=>[]);
  ref('rk',class extends Error{constructor(code){super(code);this.code=code;}});ref('tk',()=>({windows:[]}));ref('nk',()=>true);ref('ok',value=>value);ref('sk',()=>({windows:[]}));
  const names=['normalize','stringvalue','decode','tokenparse','extractaccount','account','codexheaders','header','fetch','consume','codexconfig','agheaders','project','agconfig','record','user','xaiheaders','xaiheader','paidheaders','billingfetch','paidfetch','xaiconfig','claudeheaders','claudeconfig','kimiheaders','kimiconfig','devinfactory','devinfetch','devinconfig'];
  for(const name of names){const node=input.nodes[name];vm.runInContext((node.startsWith('function ')?'':'var ')+node+';',context);}
  h.config={};for(const [provider,name] of Object.entries({codex:'codexconfig',antigravity:'agconfig',xai:'xaiconfig',claude:'claudeconfig',kimi:'kimiconfig',devin:'devinconfig'}))h.config[provider]=context[input.names[name]];
  h.api=api;h.quota=quota;h.clearCalls=clear;h.context=context;
  h.flush=async()=>{for(const [id,t]of [...timers])if(t.ms===500){timers.delete(id);t.fn();}await settle();};
  h.refresh=async()=>{events.get('online')();await settle();};h.puts=()=>calls.filter(c=>c.options.method==='PUT');h.queued=()=>[...timers.values()].some(t=>t.ms===500);
  h.success=(generation,provider='codex',name=provider+'.json',state=states[provider])=>h.commit(generation,()=>h.update(provider,previous=>({...previous,[name]:structuredClone(state)})),name.split('\0')[0]);
  h.run=(provider,item,action='fetchQuota')=>h.config[provider][action](item,key=>key);
  const auth=store({isAuthenticated:true,connectionStatus:'connected',apiBase:'http://localhost',managementKey:'secret'});
  assert.equal(api.attach({quotaStore:quota,authStore:auth}),true);
  return h;
}
async function codexCases() {
  const h=setup();await settle();const stale=file('codex',{id_token:{chatgpt_account_id:'A'},account_id:'B'}),fresh=file();
  const capture=h.capture('codex.json');await h.run('codex',stale);assert.equal(h.providerCalls.at(-1).header['Chatgpt-Account-Id'],'A');assert.equal(h.providerCalls.at(-1).authIndex,'i');
  const state=h.quota.getState();h.success(capture);assert.equal(h.queued(),false,'stale actual selector must not queue');assert.equal(h.quota.getState().codexQuota['codex.json'].status,'success');await h.flush();assert.equal(h.puts().length,0);assert.equal(h.clearCalls.length,0);assert.equal(h.quota.getState().fileGenerations,state.fileGenerations);
  await h.run('codex',fresh);h.success(capture);await h.flush();assert.equal(h.puts().length,0,'later B must not restamp old capture');
  await h.refresh();h.success(capture);await h.flush();assert.equal(h.puts().length,0,'ordinary refresh must not reset source fences');
  const latest=h.capture('codex.json');await h.run('codex',fresh);h.success(latest);await h.flush();assert.equal(h.puts().length,1);
  const entry=JSON.parse(h.puts()[0].options.body).entries[0];assert.deepEqual(Object.keys(entry).sort(),['provider','key','account','account_kind','observed_at','state'].sort());assert.equal(entry.account,'acct-codex');assert.equal(entry.account_kind,'email');assert.ok(!h.puts()[0].options.body.includes('auth_index'));assert.ok(!h.puts()[0].options.body.includes('selector_hashes'));
  // Exact actual resolver precedence: account_id is NOT a selector alias.
  for(const variant of [{account_id:'B',id_token:{chatgpt_account_id:'A'}},{chatgpt_account_id:'A',id_token:{chatgpt_account_id:'B'}},{id_token:'head.'+Buffer.from(JSON.stringify({chatgpt_account_id:'A'})).toString('base64url')+'.sig'},{metadata:{chatgptAccountId:'A'},id_token:null}]) {
    const c=h.capture('codex.json');await h.run('codex',file('codex',variant));h.success(c);await h.flush();assert.equal(h.puts().length,1);
  }
  // Already queued then mismatched before the debounce must not PUT.
  const q=h.capture('codex.json');await h.run('codex',fresh);h.success(q);await h.run('codex',stale);await h.flush();assert.equal(h.puts().length,1);
  // Native config reset awaits consume then resolves LEXICAL wrapped fetch.
  for(const item of [stale,fresh]){const c=h.capture('codex.json'),before=h.providerCalls.length;await h.run('codex',item,'resetQuota');assert.deepEqual(h.providerCalls.slice(before).map(x=>x.url),['consume','usage']);h.success(c);await h.flush();}
  assert.equal(h.puts().length,2);
  for(const variant of [{auth_index:'wrong',authIndex:'i'},{auth_index:'',authIndex:'i'},{auth_index:null,authIndex:'wrong'}]){const c=h.capture('codex.json');try{await h.run('codex',file('codex',variant));}catch{}h.success(c);await h.flush();assert.equal(h.puts().length,2);}
  const numeric=h.capture('codex.json');h.bindings=h.bindings.map(x=>x.provider==='codex'?{...x,account:'numeric-account',auth_index:'7'}:x);await h.refresh();const n=h.capture('codex.json');await h.run('codex',file('codex',{auth_index:7,authIndex:'wrong'}));h.success(n);await h.flush();assert.equal(h.puts().length,3);assert.equal(h.success(numeric),false);
}
async function unknownCases() {
  const invalid=[undefined,null,{v:2,selector_hashes:{account_id:digest('B')}},{v:1,selector_hashes:{account_id:'bad'}},{v:1,selector_hashes:{project_id:digest('B')}},{v:1,selector_hashes:{account_id:digest('B'),future:digest('B')}},{v:1,selector_hashes:{account_id:digest('B')},future:true}];
  for(const p of invalid){const h=setup();h.bindings=h.bindings.map(x=>x.provider==='codex'?{...x,manual_source_proof:p}:x);await settle();const c=h.capture('codex.json');await h.run('codex',file());h.success(c);await h.flush();assert.equal(h.puts().length,0);assert.equal(h.providerCalls.length,1);}
  // Unknown filename cannot accidentally clear a sibling native UI; global fence only.
  const h=setup();await settle();const c=h.capture('claude.json');const wrapped=h.api.wrapQuotaSource(()=>42,()=>({provider:'codex',name:null,index:'i'}));assert.equal(wrapped(),42);h.success(c,'claude');await h.flush();assert.equal(h.puts().length,0);assert.equal(h.clearCalls.length,0);
}
async function unicodeAndProofRefresh() {
  const h=setup();await settle();const old=h.capture('codex.json'),value='账户🛰️é';
  h.bindings=h.bindings.map(x=>x.provider==='codex'?{...x,manual_source_proof:proof('account_id',value)}:x);
  await h.refresh();assert.equal(h.clearCalls.length,0,'proof-only change must not clear original state');
  await h.run('codex',file('codex',{id_token:{chatgpt_account_id:value}}));h.success(old);await h.flush();assert.equal(h.puts().length,0,'proof refresh may fence, never restamp a capture');
  const c=h.capture('codex.json');await h.run('codex',file('codex',{id_token:{chatgpt_account_id:value}}));h.success(c);await h.flush();assert.equal(h.puts().length,1);assert.equal(h.providerCalls.at(-1).header['Chatgpt-Account-Id'],value);assert.ok(!h.puts()[0].options.body.includes(value));assert.ok(!h.puts()[0].options.body.includes(digest(value)));
}
async function batchCases() {
  const h=setup();h.bindings.push(who('codex','other.json'));await settle();const batch=h.capture();
  await Promise.all([h.run('codex',file('codex',{id_token:{chatgpt_account_id:'A'}})),h.run('codex',file('codex',{name:'other.json'})),h.run('claude',file('claude'))]);
  for(const [provider,name]of [['codex','codex.json'],['codex','other.json'],['claude','claude.json']])h.update(provider,previous=>{const next={...previous};assert.equal(h.commit(batch,()=>{next[name]=structuredClone(states[provider]);},name),true);return next;});
  await h.flush();assert.deepEqual(JSON.parse(h.puts()[0].options.body).entries.map(e=>e.key).sort(),['claude.json','other.json']);
  const old=h.capture('codex.json'),newer=h.capture('codex.json');await h.run('codex',file('codex',{id_token:{chatgpt_account_id:'A'}}));await h.run('codex',file());h.success(old);h.success(newer);await h.flush();assert.equal(h.puts().length,1,'overlaps cannot revive either old token');
  // Distinct native operations touching one returned map remain ambiguous.
  const a=h.capture('other.json'),b=h.capture('other.json');await h.run('codex',file('codex',{name:'other.json'}));h.update('codex',previous=>{const next={...previous};h.commit(a,()=>{next['other.json']=states.codex;});h.commit(b,()=>{next['other.json']={...states.codex};});return next;});await h.flush();assert.equal(h.puts().length,1);
}
async function agCases() {
  const h=setup();await settle();const old=h.capture('antigravity.json');await h.run('antigravity',file('antigravity',{project_id:'A'}));assert.equal(JSON.parse(h.providerCalls.at(-1).data).project,'A');h.success(old,'antigravity');await h.flush();assert.equal(h.puts().length,0);
  await h.run('antigravity',file('antigravity'));h.success(old,'antigravity');await h.flush();assert.equal(h.puts().length,0);
  for(const variant of [{project_id:'B'},{project_id:undefined,metadata:{project_id:'B'}},{project_id:undefined,attributes:{gemini_virtual_project:'B'}},{project_id:undefined}]){const c=h.capture('antigravity.json');await h.run('antigravity',file('antigravity',variant));h.success(c,'antigravity');await h.flush();}
  assert.equal(h.puts().length,4);assert.equal(h.downloads.length,1,'actual resolver called only once per fetch');
  // Downloaded project is observed, no entry-only inference. Pending gate exists
  // before call and blocks optional persistence without delaying original work.
  let release;h.downloadPromise=new Promise(resolve=>{release=resolve;});const c=h.capture('antigravity.json'),promise=h.run('antigravity',file('antigravity',{project_id:undefined}));h.success(c,'antigravity');await h.flush();assert.equal(h.puts().length,4);release(JSON.stringify({web:{project_id:'A'}}));await promise;h.success(c,'antigravity');await h.flush();assert.equal(h.puts().length,4);h.downloadPromise=null;
  // Two unresolved actual resolvers conservatively fence both operations.
  let resolve;h.downloadPromise=new Promise(r=>{resolve=r;});const first=h.capture('antigravity.json'),one=h.run('antigravity',file('antigravity',{project_id:undefined}));const second=h.capture('antigravity.json'),two=h.run('antigravity',file('antigravity',{project_id:undefined}));resolve('{"project_id":"B"}');await Promise.all([one,two]);h.success(first,'antigravity');h.success(second,'antigravity');await h.flush();assert.equal(h.puts().length,4);h.downloadPromise=null;
  const fresh=h.capture('antigravity.json');await h.run('antigravity',file('antigravity'));h.success(fresh,'antigravity');await h.flush();assert.equal(h.puts().length,5);
}
async function otherProviders() {
  const h=setup();await settle();let c=h.capture('xai.json');await h.run('xai',file('xai',{sub:'A',user_id:'B'}));assert.equal(h.providerCalls[0].header['x-userid'],'A');h.success(c,'xai');await h.flush();assert.equal(h.puts().length,0);
  c=h.capture('xai.json');await h.run('xai',file('xai'));h.success(c,'xai');await h.flush();assert.equal(h.puts().length,1);
  // Paid branch has actual fixed token headers and needs exact index, not user proof.
  h.bindings=h.bindings.map(x=>x.provider==='xai'?{...x,manual_source_proof:undefined}:x);await h.refresh();c=h.capture('xai.json');const before=h.providerCalls.length;await h.run('xai',file('xai',{paid:true,sub:'A'}));assert.ok(h.providerCalls.slice(before).every(x=>!Object.hasOwn(x.header,'x-userid')));h.success(c,'xai');await h.flush();assert.equal(h.puts().length,2);
  // An unpaid unknown selector falling back to paid health stays conservatively fenced.
  h.billingFailure=true;c=h.capture('xai.json');await h.run('xai',file('xai'));h.success(c,'xai');await h.flush();assert.equal(h.puts().length,2);
  for(const provider of ['claude','kimi','devin']) {
    const key=provider==='devin'?'devin.json\0i':provider+'.json';c=h.capture(provider+'.json');await h.run(provider,file(provider,{auth_index:'i',authIndex:provider==='devin'?'i':'wrong'}));h.success(c,provider,key);await h.flush();
    const count=h.puts().length;c=h.capture(provider+'.json');await h.run(provider,file(provider,{auth_index:provider==='devin'?'i':'wrong',authIndex:provider==='devin'?'wrong':'i'}));h.success(c,provider,key);await h.flush();assert.equal(h.puts().length,count,provider+' exact precedence');
  }
  assert.equal(h.puts().length,5);
}
async function exactSemantics() {
  const h=setup();await settle();
  const compiled=h.run('codex',file());assert.equal(compiled,h.sourceReturns.at(-1),'compiled gk returns its original Promise');await compiled;
  const project=h.context[input.names.project](file('antigravity'));assert.equal(project,h.sourceReturns.at(-1),'compiled pO returns its original Promise');await project;
  const receiver={},sentinel={},failure={},source=()=>({provider:'codex',name:'codex.json',index:'i',selectors:{account_id:'B'}});
  let args,originalThis;function original(first='default'){originalThis=this;args=[...arguments];return first==='throw'?(()=>{throw failure;})():sentinel;}
  const wrapped=h.api.wrapQuotaSource(original,source);assert.equal(wrapped.call(receiver),sentinel);assert.equal(originalThis,receiver);assert.deepEqual(args,[]);assert.throws(()=>wrapped('throw'),error=>error===failure);
  const promise=Promise.resolve(sentinel);assert.equal(h.api.wrapQuotaSource(()=>promise,source)(),promise);
  const ag=()=>({provider:'antigravity',name:'antigravity.json',index:'i',mode:'async-result'});let complete;const actual=new Promise(resolve=>{complete=resolve;});const capture=h.capture('antigravity.json');const observed=h.api.wrapQuotaSource(function(){h.success(capture,'antigravity');return actual;},ag)();assert.equal(observed,actual);await h.flush();assert.equal(h.puts().length,0);complete('B');await observed;await settle();
  assert.throws(()=>h.api.wrapQuotaSource(()=>{throw failure;},ag)(),error=>error===failure);
  assert.equal(h.api.wrapQuotaSource(()=>sentinel,()=>{throw failure;})(),sentinel,'projector failure never changes original result');
}
(async()=>{await codexCases();await unknownCases();await unicodeAndProofRefresh();await batchCases();await agCases();await otherProviders();await exactSemantics();console.log('Compiled source proof: Codex, AG, XAI, token-only indices and exact wrapper semantics passed');})().catch(error=>{console.error(error);process.exitCode=1;});
