#!/usr/bin/env ruby

require 'thor'

class CLI < Thor
  include Thor::Actions

  def initialize(*args)
    super(*args)
    @curr_dir = File.expand_path('.', __dir__)
    @curr_time = Time.now.strftime('%Y-%m-%dT%H:%M:%SZ%z')
    @go_path = File.dirname(@curr_dir)
    @bin_name = 'datalab_proxy'
  end

  desc 'go [ARGS]', 'call go command'
  def go(*args)
    inside(@curr_dir) do
      run %(GOPATH="#{@go_path}" GO111MODULE=on go #{args.join(' ')})
    end
  end

  desc 'build', 'build current project'
  def build
    inside(@curr_dir) do
      run %(rm -f ./bin/#{@bin_name})
    end
    extra_options = ''
    extra_options = %(-gcflags all="-N -l") if ENV['DEBUG_BUILD'] == '1'
    go(
      'build',
      %(-ldflags "-X main.BuiltTime=#{@curr_time}"),
      extra_options,
      %(-o bin/#{@bin_name}),
      '.'
    )
  end

  desc 'linux_build', 'build linux binary'
  def linux_build
    ENV['GOOS'] = 'linux'
    ENV['GOARCH'] = 'amd64'
    build
  end
end

CLI.start if caller.empty?
