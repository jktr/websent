# websent

This is a tool for quick and easy presentations.

Write your slides in markdown.  
Serve them over http as an in-browser presentation.  
Controlled via terminal.  
Doesn't need any JavaScript.

First, build or fetch the tool with:

```
$ git clone https://github.com/jktr/websent
$ cd websent && go build

$ go get -u github.com/jktr/websent
```

You can then launch the tutorial presentation
from the `./tutorial` directory and view
it at http://localhost:8080.

```
$ cd example/
$ websent --style builtin:tutorial tutorial.md
<TUI opens>
```

Note that (by default) all files in the current
directory are served over HTTP as part of the
presentation's assets. Set `--asset-dir` to avoid
leaking private files.

[Suckess' sent](https://tools.suckless.org/sent)
previously filled this tool's niche for me, but there
are some some issues with wayland, low-bandwidth
streaming, multi-headed output, and missing support
for fancier typesetting. While not in the least bit
suckless, using a browser as the rendering platform
addresses these issues somewhat.

This is an enhanced port of a tech demo originally
developed by [thelegy](https://github.com/thelegy), which he
built for a talk at [C3PB](https://c3pb.de/blog/lightning-talks-0x0c-keepassxc-mf70-cnc-webseiten-ohne-js.html).

