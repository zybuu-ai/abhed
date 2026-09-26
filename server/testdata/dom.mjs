// Minimal DOM: only what console.go's render() touches.
class TextNode {
  constructor(d=''){ this.nodeType=3; this.data=d; this.parentNode=null; }
  appendData(s){ this.data += s; }
  get textContent(){ return this.data; }
  // A text node can be swapped for an element, which is how the streamed
  // draft becomes the rendered answer.
  replaceWith(n){
    const p = this.parentNode;
    if(!p) return;
    p.childNodes[p.childNodes.indexOf(this)] = n;
    n.parentNode = p;
  }
}
class El {
  constructor(tag){ this.tag=tag; this.nodeType=1; this.className=''; this.childNodes=[];
    this.dataset={}; this.style={cssText:''}; this.attrs={}; this.parentNode=null; this.id=''; }
  appendChild(c){ c.parentNode=this; this.childNodes.push(c); return c; }
  append(...cs){ cs.forEach(c=>this.appendChild(c)); }
  set textContent(v){ this.childNodes=[new TextNode(String(v))]; }
  set innerHTML(v){ this._html = String(v); this.childNodes=[new TextNode(stripTags(String(v)))]; }
  get innerHTML(){ return this._html || ''; }
  get textContent(){ return this.childNodes.map(c=>c.textContent||'').join(''); }
  setAttribute(k,v){ this.attrs[k]=v; }
  get classList(){ const self=this; return {
    add:c=>{ if(!self.className.split(' ').includes(c)) self.className=(self.className+' '+c).trim(); },
    remove:(...cs)=>{ self.className=self.className.split(' ').filter(x=>x && !cs.includes(x)).join(' '); },
    replace:(a,b)=>{ const cls=self.className.split(' '); if(!cls.includes(a)) return false;
      self.className=cls.map(x=>x===a?b:x).join(' '); return true; },
    toggle:(c,on)=>{ const has=self.className.split(' ').includes(c);
      if(on===undefined?!has:on){ if(!has) self.className=(self.className+' '+c).trim(); }
      else self.className=self.className.split(' ').filter(x=>x!==c).join(' '); },
    contains:c=>self.className.split(' ').includes(c) }; }
  get isConnected(){ let n=this; while(n.parentNode) n=n.parentNode; return n===globalThis.__root; }
  replaceWith(n){ const p=this.parentNode; if(!p) return;
    p.childNodes[p.childNodes.indexOf(this)]=n; n.parentNode=p; }
  querySelector(sel){ return this.querySelectorAll(sel)[0]||null; }
  querySelectorAll(sel){
    const out=[]; const want=sel.replace(/^\./,'').split(':')[0];
    const neg=/:not\(\.([\w-]+)\)/.exec(sel);
    const walk=n=>{ for(const c of n.childNodes){ if(c.nodeType!==1) continue;
      const cls=c.className.split(' ');
      if(cls.includes(want) && (!neg || !cls.includes(neg[1]))) out.push(c);
      walk(c); } };
    walk(this); return out;
  }
  get scrollTop(){ return 0; } set scrollTop(_){ }
  get scrollHeight(){ return 0; }
}
globalThis.document = {
  createElement: t => new El(t),
  createTextNode: d => new TextNode(d),
  addEventListener(){},
};
export { El, TextNode };

// stripTags gives the visible text of rendered markup, so an assertion about
// what a reader sees does not have to parse HTML.
function stripTags(s){ return s.replace(/<[^>]*>/g, ''); }
