#!/usr/bin/perl
use v5.34;
use warnings;

use File::Spec;
use POSIX ();

my $HELP = "Usage: $0 /path/to/absolute/llm-session-search [OPTIONS]\n";

my @cmd = @ARGV;
if (!@cmd or $cmd[0] eq "-h" or $cmd[0] eq "--help") {
    die $HELP;
}
if (!File::Spec->file_name_is_absolute($cmd[0])) {
    die "ERROR: must specify the **absolute** path of llm-session-search command\n";
}

my $home = <~>;
my $dir = "$home/.llm-session-search";
if (!-d $dir) {
    mkdir $dir, 0700 or die "$dir: $!\n";
}
my $logfile = "$dir/app.log";
my $pidfile = "$dir/app.pid";

if (fork) {
    sleep 2;
    my @logline = do { open my $fh, "<", $logfile or die; <$fh> };
    print @logline;
    STDOUT->flush;
    my $pid = do { open my $fh, "<", $pidfile or die; my $pid = <$fh>; chomp $pid; $pid };
    die unless $pid;
    if (kill 0 => $pid) {
        print "\e[32mSuccessfully running, pid $pid\e[m\n";
        exit;
    } else {
        warn "\e[31mExit too early\e[m\n";
        exit 1;
    }
}
fork and exit;

POSIX::setsid();
chdir $dir or die "chdir $dir: $!\n";
{
    open my $fh, ">", $pidfile or die;
    print {$fh} "$$\n";
    close $fh;
}
open STDOUT, ">>", $logfile;
open STDERR, ">&", \*STDOUT;
open STDIN, "</dev/null";
exec {$cmd[0]} @cmd;
exit 255;
