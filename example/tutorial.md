# websent tutorial
Welcome!


- Write your slides in markdown
- Slides are separated by two blank lines


Markdown Hint: Use two spaces at the  
end of a line to force a line break.


Run a presentation (like this one)  
with `$ websent tutorial.md`  
and view it at  
[http://localhost:8080](http://localhost:8080)


Get your slides to your viewers by

- showing your browser on a beamer
- letting them browse the `websent` server  
  (see --addr/--port)


The TUI controls the presentation.  
It also previews the next few slides.


Use `j` and `k` to move between slides.  
`g` and `G` jump to first & last slide.  
Quit with `q`.  


Reload your slides and styles with `r`  
Hint: It's helpful when authoring slides.


Mouse clicks also move between slides.  
Hint: wireless mouse ≆ DIY presenter


ハロー・ワールド  
♈♉♊♋♌♍♎♏  
🜀🜁🜂🜃🜄🜅🜆🜇🜈🜉

- (non-RTL) Unicode works
- *each* connected browser needs the fonts
- your terminal should support unicode


You can provide a custom `-stylesheet`  
This one is optimized for 40 columns.


Images are served from the `-asset-dir`  
Symlinks in that directory are followed.


![green square](green.jpg)

e.g. !\[green square](green.jpg)  
Note the relative path.  


```
#!/usr/bin/env bash

echo "code blocks are supported, too"
echo "but syntax highlighting isn't (yet)"
```


Consider using a reverse proxy for TLS.  
`websent` has a `/health` endpoint.


That's all for now.

Now go try it out yourself!
