# Nightscout API Compatibility Tests

This repo is designed to compare "Nightscout compatible" API's with the original nightscout to identify issues, that may be causing errors.
FWIW, I don't think that any service should replicate the original nightscout API, but replicating enough to work with most major clients, AAPS/Loop/SugarPixel should be enough.

# Setup

1. RUN `docker-compose -f nightscout.compose.yml up`: this starts an ephemeral nightscout instance that is cleaned with each restart.
2. RUN `go run main.go nightscout`: This goes through the `*.http` files and runs each in order. 

# HTTP files

The HTTP files describe a series of requests like:

```
NAME create_entries_v3
POST http://{{ .site }}/api/v3/entries
B {"sgv": 100, "type": "sgv", "app": "test-app", "dateString": "{{ time 0 }}" }
H Authorization: Bearer {{ jwt }}
```

It uses Go templates to replace parts of the request, e.g. `{{ jwt }}` replaces with jwt token, and `{{ time 0 }}` replaces with a time string `0` minutes ago.

You can also reference previous requests output like:

```
NAME get_entries_v3
GET http://{{ .site }}/api/v3/entries/{{ dig "create_entries_v3.identifier"  }}
H Authorization: Bearer {{ jwt }}
```

Where `{{ dig "create_entries_v3.identifier"  }}` will do a json search through the previous output to get the identifier.


# Other Services
You will see the output from `nighttroop` (still in development)

You can skip the auth (since your service may do it differently and then just )


# Comparison

If you run:

```
go run diff/diff.go results/nightscout results/nighttroop
```

This will compare response bodies, status and headers for each METHOD and PATH pair to compare all results, e.g.

```
GET      /api/v3/profile        Status Score 100.00   Body Score 96.31    Overall Score 98.89    Duration200Diff -14    Total Tests 17
POST     /api/v3/profile        Status Score 100.00   Body Score 94.33    Overall Score 98.30    Duration200Diff -1     Total Tests 1
PUT      /api/v3/profile        Status Score 50.00    Body Score 86.75    Overall Score 61.02    Duration200Diff 0      Total Tests 2
PATCH    /api/v3/profile        Status Score 0.00     Body Score 89.00    Overall Score 26.70    Duration200Diff -3     Total Tests 1
DELETE   /api/v3/profile        Status Score 90.00    Body Score 94.50    Overall Score 91.35    Duration200Diff -2     Total Tests 2
GET      /api/v3/treatments     Status Score 99.20    Body Score 96.19    Overall Score 98.30    Duration200Diff -44    Total Tests 25
POST     /api/v3/treatments     Status Score 97.06    Body Score 93.69    Overall Score 96.05    Duration200Diff -94    Total Tests 34
PUT      /api/v3/treatments     Status Score 100.00   Body Score 92.83    Overall Score 97.85    Duration200Diff -8     Total Tests 3
PATCH    /api/v3/treatments     Status Score 100.00   Body Score 87.50    Overall Score 96.25    Duration200Diff -2     Total Tests 2
DELETE   /api/v3/treatments     Status Score 100.00   Body Score 100.00   Overall Score 100.00   Duration200Diff -5     Total Tests 2
GET      /api/v3/version        Status Score 100.00   Body Score 99.33    Overall Score 99.80    Duration200Diff -1     Total Tests 1
TOTAL                           Status Score 97.93    Body Score 93.05    Overall Score 96.47    Duration200Diff -627   Total Tests 232
```

These numbers are just heuristics, and could be improved greatly.

The goal of this report is not to get services to be 100% compatible, but highlight the differences between OG Nightscout.

# TODO

1. Have service specific read and write queries so that we can check compatibility with say AAPS. For exmaple, if AAPS uses PATCH /api/v3/treatments in a specific way, then we can encode and make sure responses are in the right shape.
2. Test Web services API, a whole other can of worms.
3. Add nocturne
