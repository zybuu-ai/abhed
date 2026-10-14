// A model as the review holds it: its lines, set whole by an edit.
const model = text => ({ text, getValue(){ return this.text; }, getLinesContent(){ return this.text === '' ? [] : this.text.split('\n'); },
  getEOL(){ return '\n'; }, getFullModelRange(){ return null; }, pushEditOperations(_, ops){ this.text = ops[0].text; } });
let failed = 0;
const check = (ok, what) => { if(!ok){ failed++; console.log('FAIL ' + what); } else console.log('ok   ' + what); };
const reset = () => { __posted.length = 0; __saved.length = 0; __closed.length = 0; };

// The only change of an added file: rejecting it removes the file.
let t = {id:'t1', path:'new.txt', existed:false, orig:model(''), model:model('one\ntwo')};
await rejectHunk(t, {originalStartLineNumber:0, originalEndLineNumber:0, modifiedStartLineNumber:1, modifiedEndLineNumber:2});
check(__posted.length === 1 && __posted[0].url.endsWith('/delete') && __posted[0].body.path === 'new.txt', 'an added file whose only change is rejected is removed');
check(__saved.length === 0 && __closed.includes('t1'), 'and is not saved empty');

// One of two changes in an added file the person edited: the rest stays.
reset();
t = {id:'t2', path:'grown.txt', existed:false, orig:model(''), model:model('keep\ndrop')};
await rejectHunk(t, {originalStartLineNumber:0, originalEndLineNumber:0, modifiedStartLineNumber:2, modifiedEndLineNumber:2});
check(__posted.length === 0 && __saved.length === 1 && t.model.getValue() === 'keep', 'an added file with text left is saved, not removed');

// A file that existed is restored and saved, never removed.
reset();
t = {id:'t3', path:'old.txt', existed:true, orig:model('a'), model:model('a\nb')};
await rejectHunk(t, {originalStartLineNumber:1, originalEndLineNumber:0, modifiedStartLineNumber:2, modifiedEndLineNumber:2});
check(__posted.length === 0 && __saved.length === 1 && t.model.getValue() === 'a', 'a file that existed is restored and saved');
if(failed) process.exit(1);
