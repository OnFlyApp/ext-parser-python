try:
    import ujson as json
except ImportError:
    import json

if TYPE_CHECKING:
    from foo import Bar

    class Proto:
        def run(self): ...
else:
    def shim():
        pass

x = 1  # not a doc comment
def after():
    pass
