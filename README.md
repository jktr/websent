# websent

This is a tool for quick and easy presentations.

Write your slides in markdown.  
Serve them over http as an in-browser presentation.  
Controlled via CLI. Doesn't need JavaScript.

First, install the tool with:
```
$ go get -u github.com/jktr/websent
```

You can then run the tool like this:
```
$ websent --port 8080 presentation.md
```

The file format for presentations looks like this:
```markdown
# example presentation


This is the first slide.


- This is the the second slide
- Slides are separated by two blank lines


Run with `websent slides.md` and browse
to [localhost](http://localhost:8080).


Use `j` and `k` move between slides.
`r` reloads. `q` quits.
```

Note:
- You will probably want to provide your own stylesheet.
- There is no TLS support. Use a reverse proxy if you need it.

[Suckess' sent](https://tools.suckless.org/sent)
previously filled this tool's niche for me, but there
are some some issues with wayland, low-bandwidth
streaming, and missing support for fancier typesetting.
While not in the least bit suckless, using a
browser as the rendering platform addresses
these issues somewhat.

This is an enhanced port of a tech demo originally
developed by [thelegy](https://github.com/thelegy), which he
built for a talk at [C3PB](https://c3pb.de/blog/lightning-talks-0x0c-keepassxc-mf70-cnc-webseiten-ohne-js.html).

