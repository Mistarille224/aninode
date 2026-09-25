import test from "node:test";
import assert from "node:assert/strict";
import {readFileSync} from "node:fs";
import {discoveryDisplayOrder,readDiscoveryOrder,rssObservationPresentation,filterEditor,sourceSettings,buildOutput,buildEntryDeclaration,buildMovieClassify,movieClassifyText,blacklistText,bindEntryItems,previewPresentation,seasonPresentation,setupComplete,uiCapabilities,discoverySources,previewNameValid,clearEntrySelection,renderSeasonCards,renderRepairReviews,renderFolderCards,renderMigration,migrationSeasonField,migrationDecisionPresentation,migrationActionSummary,migrationConfirmationModel,bindMigrationPlanControls,migrationPlan,entrySummaryPresentation,projectionFromResponses,buildSourcePayload,issueText,renderIssues,bind,saveEntryWorkflow,migrationApplyWorkflow,safeExternalURL,newClientModel,loginPage} from "./app.js";
test("operation issues render the same message and actionable hint",()=>{const issue={severity:"error",stage:"publication",code:"publication_conflict",message:"destination differs",entry:"movie/x",hint:"inspect source and destination"};assert.equal(issueText(issue),"destination differs — inspect source and destination");const html=renderIssues([issue]);assert.match(html,/destination differs/);assert.match(html,/inspect source and destination/);assert.match(html,/movie\/x/)});
test("RSS discovery keeps every observed source while search binds only the selected source",()=>{assert.deepEqual(discoverySources({source_ids:["dmhy","nyaa"]}),["dmhy","nyaa"]);assert.deepEqual(discoverySources({source_ids:["dmhy","nyaa"]},"nyaa"),["nyaa"])});
test("typical nullable management payload renders season cards",()=>{assert.doesNotThrow(()=>renderSeasonCards([{season:1,inventory_known:true,present_episodes:null,missing:null,status:"unknown"}]));assert.match(renderSeasonCards([{season:1,inventory_known:false,inventory_error:"permission denied",present_episodes:null,missing:null,status:"unknown"}]),/permission denied/)});

test("low-confidence repair renders an explicit confirmation instead of looking automatic",()=>{
  const review={id:"abc",season:1,episode:6,source_id:"mikan",expected_group:"grpabc01",reason:"Manual confirmation required",release:{provider:"mikan",media_name:"[grpabc02] abcabc - 06 [WEB-DL 1080p AVC AAC].mkv",group:"grpabc02",resolution:"1080p"},differences:[{field:"Release group",expected:"grpabc01",candidate:"grpabc02"}]};
  const html=renderRepairReviews([review]);
  assert.match(html,/Episode awaiting confirmation/);
  assert.match(html,/grpabc01/);
  assert.match(html,/grpabc02/);
  assert.match(html,/data-confirm-repair="abc"/);
  assert.match(html,/Use this version/);
  const season=renderSeasonCards([{season:1,inventory_known:true,present_episodes:[3,4,5,7],missing:[6],status:"incomplete",repair_reviews:[review]}]);
  assert.match(season,/Pending confirmation 1/);
  assert.match(season,/low-confidence completion candidate\(s\) await confirmation/);
  const range=renderRepairReviews([{...review,episode_start:5,episode_end:6}]);
  assert.match(range,/E05–E06/);
});
test("continuous filesystem state needs no remote history",()=>{const s={season:1,inventory_known:true,present_episodes:[1,2,3],missing:[],status:"continuous"};assert.deepEqual(seasonPresentation(s),{kind:"continuous",text:"Current files are continuous"});const html=renderSeasonCards([s]);assert.match(html,/Current files are continuous/);assert.doesNotMatch(html,/Pending confirmation|Latest releases/)});
test("movie preview requires a name before requesting",()=>{assert.equal(previewNameValid(""),false);assert.equal(previewNameValid("   "),false);assert.equal(previewNameValid("abcabc.mkv"),true)});
test("starting an entry search clears selected editor state",()=>{const s={entry:{entry:{key:"series/abcabc00"}},seasons:[{season:1}]};clearEntrySelection(s);assert.equal(s.entry,null);assert.deepEqual(s.seasons,[])});
test("setup and source capabilities are independent",()=>{const c={organizer:{source:"/downloads",target:"/library"},clients:{c:{enabled:true}},sources:{m:{id:"m",enabled:true}},source_capabilities:{m:{discovery:false}}};assert.equal(setupComplete(c),true);assert.deepEqual(uiCapabilities(c),{roots:true,clients:true,sources:true})});
test("series output only stores title and blacklist remains line based",()=>{assert.deepEqual(buildOutput(" abcabc "),{title:"abcabc"});const d=buildEntryDeclaration({},{output_title:"abcabc",blacklist:"*NCOP*\n*sample*"});assert.deepEqual(d.blacklist,["*NCOP*","*sample*"]);assert.deepEqual(d.output,{title:"abcabc"})});
test("entry items open through direct binding and delegated fallback",()=>{const opened=[],row={dataset:{entryId:"initial"},onclick:null},container={contains:()=>true,onclick:null,querySelectorAll:()=>[row]};bindEntryItems(container,id=>opened.push(id));row.onclick({stopPropagation(){}});container.querySelectorAll=()=>[];bindEntryItems(container,id=>opened.push(id));container.onclick({target:{closest:()=>({dataset:{entryId:"filtered"},onclick:null})}});assert.deepEqual(opened,["initial","filtered"])});
test("blacklist textarea round trips",()=>{const values=["*NCOP*","SP/*"];assert.equal(blacklistText(values),"*NCOP*\nSP/*");assert.deepEqual(buildEntryDeclaration({}, {output_title:"",blacklist:blacklistText(values)}).blacklist,values)});
test("movie classification overrides round trip stable relative paths",()=>{const value="Extras/abcabc01.mkv = extra\nabcabc.DC.mkv = version";const classify=buildMovieClassify(value);assert.deepEqual(classify,{"Extras/abcabc01.mkv":"extra","abcabc.DC.mkv":"version"});assert.equal(movieClassifyText(classify),value);assert.deepEqual(buildEntryDeclaration({}, {output_title:"abcabc",blacklist:"",movie_classify:value}).movie.classify,classify)});
test("migration movie exposes classification when backend reports the real blocker",()=>{const html=renderMigration({groups:[{key:"m",title:"abcabc05",media_type:"movie",blocked:1,plans:[{task_id:"t",decision:"conflict",detail:"movie bundle requires classification: secondary video has no reliable marker"}]}]});assert.match(html,/classify the main feature, extras, and similar files/);assert.match(html,/data-migration-classify/);assert.match(html,/movie bundle requires classification/)});
test("movie auto classification removes an explicit override",()=>{assert.deepEqual(buildMovieClassify("abcabc01.mkv = auto\nabcabc.DC.mkv = version"),{"abcabc.DC.mkv":"version"})});
test("preview presentation explains exclusion and mapping",()=>{const excluded=previewPresentation({decision:"excluded",reason:'matched Entry blacklist pattern "*NCOP*"'});assert.equal(excluded.title,"Excluded");assert.equal(excluded.rule,"*NCOP*");const mapped=previewPresentation({decision:"selected",parsed:{title:"ABCABC"},output_title:"AlphaBeta",source_episode:{season:1,episode_start:13},target_episode:{season:2,episode_start:1},expected_path:"/library/AlphaBeta/Season 02/AlphaBeta S02E01.mkv"});assert.equal(mapped.sourceTitle,"ABCABC");assert.equal(mapped.outputTitle,"AlphaBeta");assert.equal(mapped.source.episode_start,13);assert.equal(mapped.target.episode_start,1)});

test("migration scan controls always bind a concrete handler",()=>{
  const calls=[];
  const button={addEventListener:(type,handler)=>calls.push({type,handler})};
  const root={querySelectorAll:selector=>selector==="[data-migration-plan]"?[button,button]:[]};
  assert.equal(typeof migrationPlan,"function");
  assert.equal(bindMigrationPlanControls(root,migrationPlan),2);
  assert.equal(calls.length,2);
  assert.ok(calls.every(x=>x.type==="click"&&x.handler===migrationPlan));
});
test("page binder resolves every referenced handler without a browser",()=>{
  const previous=globalThis.document;
  globalThis.document={querySelector:()=>null,querySelectorAll:()=>[]};
  try{assert.doesNotThrow(()=>bind())}finally{if(previous===undefined)delete globalThis.document;else globalThis.document=previous}
});
test("migration presentation keeps movie seasonless and series seasonal",()=>{const plan={task_id:"t",task_name:"relabc",client:"c",decision:"unknown",detail:"backend topology detail"};const movie=renderMigration({groups:[{key:"m",title:"ghighi",season:1,media_type:"movie",ready:0,blocked:1,plans:[plan]}]});assert.doesNotMatch(movie,/data-migration-season /);assert.match(movie,/Year \(optional\)/);assert.doesNotMatch(movie,/data-migration-hint|media library target：/);assert.match(movie,/Verify the title and type/);assert.match(movie,/<summary>Technical details<\/summary>/);assert.match(movie,/backend topology detail/);const series=renderMigration({groups:[{key:"s",title:"abcabc",season:2,media_type:"series",ready:0,blocked:1,plans:[plan]}]});assert.match(series,/data-migration-season /);assert.match(series,/value="2"/);assert.match(series,/Year \(optional\)/);assert.match(series,/Verify the title and type/)});
test("migration type switching creates a reasonable series season and removes movie season",()=>{assert.match(migrationSeasonField("migration-0","series"),/value="1"/);assert.equal(migrationSeasonField("migration-0","movie",7),"");assert.match(migrationSeasonField("migration-0","series",7),/value="7"/);assert.match(migrationSeasonField("migration-0","series",0),/value="0"/)});
test("migration decisions distinguish executable and blocked plans",()=>{assert.equal(migrationDecisionPresentation("adopt").ready,true);assert.equal(migrationDecisionPresentation("relocate").ready,true);assert.equal(migrationDecisionPresentation("conflict").ready,false);assert.equal(migrationDecisionPresentation("unknown").ready,false)});
test("new-entry adopt is presented as no-move confirmation",()=>{const html=renderMigration({groups:[{key:"s",title:"abcabc",season:1,media_type:"series",ready:1,blocked:0,plans:[{task_name:"[grpabc] abcabc S01",client:"qbit",save_path:"/downloads/TV/abcabc/[grpabc] abcabc S01",decision:"adopt"}]}]});assert.match(html,/No move needed/);assert.match(html,/managed without moving existing downloaded files/);assert.doesNotMatch(html,/can be adopted directly|canonical directory/)});
test("relocate renders as executable placement adjustment",()=>{const html=renderMigration({groups:[{key:"s",title:"abcabc",season:1,media_type:"series",ready:1,blocked:0,plans:[{task_name:"batabc",client:"qbit",decision:"relocate"}]}]});assert.equal(migrationDecisionPresentation("relocate").ready,true);assert.match(html,/Relocation needed/);assert.match(html,/downloader will move the task to/)});
test("movie new-entry confirmation remains seasonless",()=>{const html=renderMigration({groups:[{key:"m",title:"ghighi",media_type:"movie",ready:1,blocked:0,plans:[{task_name:"[grpabc] defdef BDRip",client:"qbit",save_path:"/downloads/Movies/[grpabc] defdef BDRip",decision:"adopt"}]}]});assert.doesNotMatch(html,/data-migration-season /);assert.match(html,/No move needed/);assert.doesNotMatch(html,/data-migration-hint|media library target：/);assert.doesNotMatch(html,/canonical directory/)});
test("migration confirmation describes the actual backend action",()=>{const summary=migrationActionSummary([{decision:"adopt"},{decision:"adopt"},{decision:"relocate"}]);assert.deepEqual(summary,{count:3,relocate:1,adopt:2,blocked:0});const model=migrationConfirmationModel([{decision:"relocate"}],{title:"abcabc",mediaType:"series",season:2});assert.equal(model.kind,"relocate");assert.match(model.message,/relocated by the downloader/);assert.doesNotMatch(model.message,/adopt/)});
test("blocked migration confirmation is about identity resolution, not zero executable counts",()=>{const model=migrationConfirmationModel([{decision:"unknown"}],{title:"abcabc",mediaType:"series",season:2});assert.equal(model.kind,"resolve");assert.match(model.message,/abcabc.*Season 02/);assert.match(model.message,/state will be checked again before execution/);assert.doesNotMatch(model.message,/0  /)});
test("empty migration view explains that already managed tasks are intentionally absent",()=>{const html=renderMigration({groups:[]});assert.match(html,/No tasks need migration/);assert.match(html,/matching title download directory do not need migration/)});
test("migration rendering tolerates nullable payload fields",()=>{assert.doesNotThrow(()=>renderMigration(null));assert.doesNotThrow(()=>renderMigration({groups:[{plans:null}]}))});
test("migration task detail stays collapsed by default",()=>{const html=renderMigration({groups:[{key:"s",title:"abcabc",season:1,media_type:"series",blocked:1,plans:[{task_id:"t",decision:"unknown"}]}]});assert.match(html,/class="migration-grid"/);assert.match(html,/<details class="task-details">/);assert.doesNotMatch(html,/<details class="task-details" open/)});
test("migration exposes observed filter choices without selecting them for a new entry",()=>{const html=renderMigration({groups:[{key:"s",title:"abcabc",season:1,media_type:"series",ready:1,blocked:0,filter_options:{groups:["grpabc03","grpabc01"],resolutions:["1080p"],subtitles:["zh-Hans"]},plans:[{task_id:"t",decision:"adopt"}]}]});assert.match(html,/Future release filters \(optional\)/);assert.match(html,/grpabc03/);assert.match(html,/grpabc01/);assert.match(html,/1080p/);assert.match(html,/Nothing is selected by default/);assert.doesNotMatch(html,/value="grpabc03" checked/)});
test("migration preserves existing entry filters as checked choices",()=>{const html=renderMigration({groups:[{key:"s",entry_key:"series/abcabc",title:"abcabc",season:1,media_type:"series",ready:1,blocked:0,filters:{groups:["grpabc03"]},filter_options:{groups:["grpabc03","grpabc01"]},plans:[{task_id:"t",decision:"relocate"}]}]});assert.match(html,/value="grpabc03" checked/);assert.doesNotMatch(html,/value="grpabc01" checked/)});
test("folder projection preserves specials season zero",()=>{const html=renderFolderCards([{name:"Extras",path:"/downloads/TV/abcabc/Extras",suggested_season:1,projection:{season:0}}]);assert.match(html,/Season 00/);assert.match(html,/data-folder-season[^>]*value="0"/)});

test("entry editor preserves root folder projections while editing series fields",()=>{
  const base={title:"abcabc04",year:2026,folders:{"Season 01":{season:2,episode_offset:-10}},output:{title:""},blacklist:[]};
  const d=buildEntryDeclaration(base,{title:"abcabc06",year:2027,output_title:"",blacklist:""});
  assert.equal(d.title,"abcabc06");
  assert.equal(d.year,2027);
  assert.deepEqual(d.folders,{"Season 01":{season:2,episode_offset:-10}});
});


test("folder projection editor uses real folder name and absolute target season",()=>{const html=renderFolderCards([{name:"Season 2あいう",path:"/src/abcabc/Season 2あいう",suggested_season:1,projection:{season:2,episode_offset:-12}}]);assert.match(html,/Season 2あいう/);assert.match(html,/Season 02/);assert.match(html,/value="2"/);assert.match(html,/value="-12"/);assert.doesNotMatch(html,/Seasonoffset/)});


test("catalog summary presents actionable work before normal work",()=>{
  assert.deepEqual(entrySummaryPresentation({summary:{status:"incomplete",missing:2},declaration:{enabled:true}}),{kind:"incomplete",label:"Missing 2 episode(s)",text:"2 episode(s) missing"});
  assert.deepEqual(entrySummaryPresentation({summary:{status:"incomplete",missing:1,reviews:1},declaration:{enabled:true}}),{kind:"incomplete",label:"Pending confirmation 1",text:"1 low-confidence completion candidate(s)"});
  assert.equal(entrySummaryPresentation({summary:{status:"conflict",detail:"mapping conflict"},declaration:{enabled:true}}).kind,"conflict");
  assert.equal(entrySummaryPresentation({summary:{status:"complete",seasons:2},declaration:{enabled:true}}).label,"Normal");
  assert.equal(entrySummaryPresentation({summary:{status:"complete"},declaration:{enabled:false}}).label,"Disabled");
  assert.equal(entrySummaryPresentation({summary:{status:"unobserved"},declaration:{enabled:true}}).label,"Managed");
});


test("backend projection keeps an open entry while global config refreshes",()=>{
  const values=[{sources:{}},{runtime_config:"ready"},[{entry:{key:"series/abcabc",title:"abcabc"}}],{entry:{key:"series/abcabc"},declaration:{},seasons:[{season:1}],folders:[{name:"Season 01"}]}];
  const p=projectionFromResponses(values,"series/abcabc");
  assert.equal(p.entry.entry.key,"series/abcabc");
  assert.deepEqual(p.seasons,[{season:1}]);
  assert.deepEqual(p.folders,[{name:"Season 01"}]);
});


test("source payload keeps builtin search internal and supports explicit RSS overrides",()=>{
  assert.deepEqual(buildSourcePayload({provider:"mikan"},{mode:"default",enabled:true,priority:"2"}),{id:"mikan",provider:"mikan",priority:2,enabled:true});
  assert.deepEqual(buildSourcePayload({provider:"mikan"},{mode:"custom",rssURLs:[" https://a/rss ","https://a/rss","https://b/rss"],searchTemplate:"https://ignored/{query}",enabled:true}),{id:"mikan",provider:"mikan",priority:0,enabled:true,rss:[{url:"https://a/rss"},{url:"https://b/rss"}]});
});

test("generic source payload requires a real observation or search endpoint",()=>{
  assert.throws(()=>buildSourcePayload({provider:"generic"},{id:"custom",enabled:true}),/requires at least one RSS URL or a search URL/);
  assert.deepEqual(buildSourcePayload({provider:"generic"},{id:"custom",searchTemplate:"https://x/search?q={query}",enabled:true}),{id:"custom",provider:"generic",priority:0,enabled:true,search:{url_template:"https://x/search?q={query}"}});
});


test("release filters are default-open checkbox traits",()=>{
  const html=filterEditor({groups:["A"],resolutions:["1080p"]},{groups:["A","B"],resolutions:["1080p","2160p"],subtitles:["CHS"]});
  assert.match(html,/data-filter-values="groups"/);
  assert.match(html,/value="A" checked/);
  assert.match(html,/value="B"/);
  assert.match(html,/value="2160p"/);
  assert.match(html,/value="CHS"/);
  assert.match(html,/Everything is allowed by default/);
  assert.match(html,/clear the selection to allow every value/);
  assert.doesNotMatch(html,/must contain|Excludedterm|Use default |textarea/);
  const open=filterEditor({},{});
  assert.match(open,/leave everything unselected for no restriction/);
});
test("configured custom sources are visible by default",()=>{
  const html=sourceSettings({sources:{custom:{id:"custom",provider:"generic",enabled:true,rss:[{url:"https://example.test/rss"}]}},source_capabilities:{}});
  assert.match(html,/<details class="subpanel" open><summary>Custom sources \(1\)<\/summary>/);
});

test("source rows pair each provider with its own status and actions",()=>{
  const html=sourceSettings({sources:{mikan:{id:"mikan",provider:"mikan",enabled:true,rss:[{url:"https://example.test/rss"}]}},source_capabilities:{mikan:{historical:true}}});
  assert.match(html,/<strong>Mikan<\/strong><span>Enabled<\/span>/);
  assert.match(html,/1 custom RSS<span> · Historical search available<\/span>/);
  assert.match(html,/data-source-config="mikan"/);
  assert.match(html,/data-toggle-source="mikan"/);
  assert.match(html,/>Disabled<\/span>/);
});


test("RSS freshness distinguishes cold, failed, stale, and partial results",()=>{
 assert.equal(rssObservationPresentation({status:"pending"}),"Waiting for first update");
 assert.equal(rssObservationPresentation({status:"error",stale:false}),"Update failed · No available results");
 assert.match(rssObservationPresentation({status:"error",stale:true,updated_at:"2026-09-19T09:00:00Z"}),/Update failed · retain/);
 assert.match(rssObservationPresentation({status:"partial",updated_at:"2026-09-19T09:00:00Z"}),/Partial results/);
 assert.match(rssObservationPresentation({status:"ready",updated_at:"2026-09-19T09:00:00Z"}),/Updated/);
});

test("RSS rate limits retain partial status and show the retry deadline",()=>{
 const source={status:"partial",updated_at:"2026-09-19T09:00:00Z"};
 const issue={stage:"source",code:"rss_observation_failed",message:"RSS default: request returned HTTP 429; retry after 2026-09-19T09:02:00Z"};
 assert.match(rssObservationPresentation(source),/Partial results/);
 const html=renderIssues([issue]);
 assert.match(html,/HTTP 429/);
 assert.match(html,/retry after 2026-09-19T09:02:00Z/);
});

test("discovery display order preserves identity, ties and merged sources",()=>{
 const groups=[{title:"abc1",source_ids:["nyaa"]},{title:"abc2",source_ids:["mikan"]},{title:"abc3",source_ids:["nyaa","mikan"]},{title:"abc4",source_ids:null}];
 assert.deepEqual(discoveryDisplayOrder(groups,["mikan","nyaa"]).map(x=>x.i),[1,2,0,3]);
 assert.deepEqual(discoveryDisplayOrder(groups).map(x=>x.i),[0,1,2,3]);
 assert.equal(discoveryDisplayOrder(groups,["mikan"])[0].g,groups[1]);
 assert.equal(groups[0].title,"abc1");
 assert.deepEqual(discoveryDisplayOrder(null),[]);
});
test("display preferences tolerate corrupt and unavailable storage",()=>{
 for(const value of ["null","{}","invalid"]){assert.deepEqual(readDiscoveryOrder({getItem:()=>value}),[])}
 assert.deepEqual(readDiscoveryOrder({getItem:()=>{throw Error("denied")}}),[]);
 assert.deepEqual(readDiscoveryOrder({getItem:()=>'["mikan",1,"mikan","nyaa"]'}),["mikan","nyaa"]);
});


test("entry save workflow persists, reloads, and preserves selected entry identity",async()=>{
 const calls=[]; let selected="series/abcabc";
 const api=async(url,opt)=>{calls.push([url,opt.method]);return{entry:{key:selected}}};
 const reload=async()=>{assert.equal(selected,"series/abcabc");calls.push(["reload","GET"])};
 const key=await saveEntryWorkflow(api,selected,{sources:["mikan"],filters:{groups:["A"]}},reload);
 assert.equal(key,"series/abcabc"); assert.deepEqual(calls.map(x=>x[0]),["/ui/catalog/series/abcabc/declaration","reload"]);
});

test("migration workflow reports apply-success/rescan-failure and does not pretend reload completed",async()=>{
 const calls=[]; let reloaded=false;
 const api=async(url)=>{calls.push(url);if(url.endsWith("/apply"))return{applied:[{}]};throw new Error("scan unavailable")};
 await assert.rejects(()=>migrationApplyWorkflow(api,{key:"g"},async()=>{reloaded=true}),/Migration completed, but rescan failed: scan unavailable/);
 assert.deepEqual(calls,["/ui/migrations/apply","/ui/migrations/plan"]); assert.equal(reloaded,false);
});

test("external discovery links allow only http and https",()=>{
  assert.equal(safeExternalURL("https://example.test/a?b=1"),"https://example.test/a?b=1");
  assert.equal(safeExternalURL("http://example.test/path"),"http://example.test/path");
  assert.equal(safeExternalURL("javascript:alert(1)"),"");
  assert.equal(safeExternalURL("file:///etc/passwd"),"");
  assert.equal(safeExternalURL("//example.test/path"),"");
});

test("new downloader model contains only current credential fields",()=>{
  const value=newClientModel();
  assert.equal(value.credential_set,false);
  assert.equal("credential" in value,false);
  assert.equal("password" in value,false);
  assert.equal("secret" in value,false);
});


test("login page separates first setup and returning sessions without exposing credentials",()=>{
 const setup=loginPage(true);
 assert.match(setup,/Verify and set password/);
 assert.match(setup,/id="login-setup-code"/);
 assert.match(setup,/setup-code/);
 assert.match(setup,/id="login-confirm"/);
 assert.match(setup,/autocomplete="new-password"/);
 const login=loginPage(false,'<script>alert("bad")</script>');
 assert.match(login,/autocomplete="current-password"/);
 assert.doesNotMatch(login,/id="login-confirm"|<script>|API|token/i);
 assert.match(login,/&lt;script&gt;/);
});

import {translateExact,formatMessage,initI18n} from "./i18n.js";

test("i18n translates complete messages only and never mutates brand names",()=>{
  const catalog={"Content source":"内容来源","by":"由","RSS":"RSS"};
  assert.equal(translateExact("Content source",catalog),"内容来源");
  assert.equal(translateExact("Emby / Plex",catalog),"Emby / Plex");
  assert.equal(translateExact("1 custom RSS",catalog),"1 custom RSS");
});

test("zh-CN catalog loads grouped messages and reorders whole sentence placeholders",async()=>{
  const previous=Object.fromEntries(["navigator","document","MutationObserver","NodeFilter","Node","fetch"].map(key=>[key,Object.getOwnPropertyDescriptor(globalThis,key)]));
  const catalog=JSON.parse(readFileSync(new URL("./locales/zh-CN.json",import.meta.url),"utf8"));
  let onMutation;
  try{
    Object.defineProperties(globalThis,{
      navigator:{value:{languages:["zh-CN"]},configurable:true},
      document:{value:{nodeType:9,documentElement:{lang:""},createTreeWalker:()=>({nextNode:()=>null})},configurable:true},
      MutationObserver:{value:class{constructor(callback){onMutation=callback} observe(){} disconnect(){}},configurable:true},
      NodeFilter:{value:{SHOW_ELEMENT:1,SHOW_TEXT:4},configurable:true},
      Node:{value:{ELEMENT_NODE:1,TEXT_NODE:3},configurable:true},
      fetch:{value:async()=>({ok:true,json:async()=>catalog}),configurable:true},
    });
    assert.equal(await initI18n(),"zh-CN");
    assert.equal(formatMessage("Season {number}",{number:"02"}),"第 02 季");
    assert.equal(formatMessage("Set the download location and the media library directory read by Emby / Plex.",{}),"指定下载文件的位置，以及供 Emby / Plex 读取的媒体库目录。");
    assert.equal(formatMessage("{name} enabled",{name:"Emby"}),"已启用 Emby");
    const changedText={nodeType:3,nodeValue:"Passwords do not match"};
    onMutation([{type:"characterData",target:changedText,addedNodes:[]}]);
    assert.equal(changedText.nodeValue,"两次输入的密码不一致");
    globalThis.fetch=async()=>({ok:false});
    assert.equal(await initI18n(),"en");
    assert.equal(formatMessage("Season {number}",{number:"02"}),"Season 02");
  }finally{
    for(const [key,descriptor] of Object.entries(previous)){
      if(descriptor)Object.defineProperty(globalThis,key,descriptor);
      else delete globalThis[key];
    }
  }
});
