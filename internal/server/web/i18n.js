const SOURCE_LOCALE = "en";
const DEFAULT_LOCALE = SOURCE_LOCALE;
let locale = DEFAULT_LOCALE;
let messages = {};
let observer;

function preferredLocale(){
  const langs = globalThis.navigator?.languages?.length ? navigator.languages : [globalThis.navigator?.language].filter(Boolean);
  for(const value of langs){
    const lang=String(value||"").toLowerCase();
    if(lang==="en"||lang.startsWith("en-")) return SOURCE_LOCALE;
    if(lang==="zh"||lang.startsWith("zh-")) return "zh-CN";
  }
  return DEFAULT_LOCALE;
}

async function loadMessages(name){
  if(name===SOURCE_LOCALE) return {};
  try{
    const response=await fetch(`/assets/locales/${name}.json`,{cache:"no-cache"});
    if(!response.ok) return {};
    const data=await response.json();
    if(!data?.messages) return {};
    return Object.assign({},...Object.values(data.messages));
  }catch{return {}}
}

export function translateExact(value,catalog){
  const source=String(value??"");
  const leading=source.match(/^\s*/)?.[0]||"";
  const trailing=source.match(/\s*$/)?.[0]||"";
  const core=source.slice(leading.length,source.length-trailing.length);
  if(!core) return source;
  const translated=catalog?.[core];
  return translated===undefined?source:`${leading}${translated}${trailing}`;
}

export function translate(value){
  if(locale===SOURCE_LOCALE || value==null) return String(value??"");
  return translateExact(value,messages);
}

export function formatMessage(source, values={}){
  return translate(source).replace(/\{([A-Za-z0-9_]+)\}/g,(match,key)=>Object.hasOwn(values,key)?String(values[key]):match);
}

export async function initI18n(){
  locale=preferredLocale();
  messages=await loadMessages(locale);
  if(locale!==SOURCE_LOCALE&&!Object.keys(messages).length) locale=SOURCE_LOCALE;
  document.documentElement.lang=locale;
  observer?.disconnect();
  observer=new MutationObserver(records=>{
    observer.disconnect();
    for(const record of records){
      for(const node of record.addedNodes) localizeDOM(node);
      if(record.type==="characterData") localizeDOM(record.target);
      if(record.type==="attributes") localizeDOM(record.target);
    }
    observer.observe(document.documentElement,{childList:true,subtree:true,characterData:true,attributes:true,attributeFilter:["placeholder","aria-label","title"]});
  });
  observer.observe(document.documentElement,{childList:true,subtree:true,characterData:true,attributes:true,attributeFilter:["placeholder","aria-label","title"]});
  localizeDOM(document);
  return locale;
}

export function localizeDOM(root=document){
  if(locale===SOURCE_LOCALE || !root) return;
  const translateElement=el=>{
    for(const attr of ["placeholder","aria-label","title"]){
      if(el.hasAttribute?.(attr)){
        const source=el.getAttribute(attr),localized=translate(source);
        if(source!==localized) el.setAttribute(attr,localized);
      }
    }
  };
  if(root.nodeType===Node.TEXT_NODE){const localized=translate(root.nodeValue);if(root.nodeValue!==localized)root.nodeValue=localized;return}
  if(root.nodeType===Node.ELEMENT_NODE) translateElement(root);
  const walker=document.createTreeWalker(root,NodeFilter.SHOW_ELEMENT|NodeFilter.SHOW_TEXT);
  let node; while((node=walker.nextNode())){
    if(node.nodeType===Node.TEXT_NODE){const localized=translate(node.nodeValue);if(node.nodeValue!==localized)node.nodeValue=localized}
    else translateElement(node);
  }
}

export function friendlyError(message){
  const raw=String(message||"");
  let friendly=raw;
  if(/failed to fetch|networkerror when attempting to fetch/i.test(raw)) friendly="Unable to connect to server";
  else if(/client\s+""\s+unavailable|no enabled downloader|downloader.*unavailable/i.test(raw)) friendly="No downloader is enabled. Enable a downloader in Settings first.";
  else if(/no enabled.*source|content source.*disabled|content source.*not found/i.test(raw)) friendly="No content source is enabled. Enable a content source in Settings first.";
  return translate(friendly);
}

export function currentLocale(){return locale}
